package agent

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/helloxz/zlite/internal/llm"
	"github.com/helloxz/zlite/internal/session"
)

// mentionRe 匹配 @ 引用：行首或空白后的 @<token>（token 不含空白与 @）。
// 用 (^|\s) 锚定避免误伤邮箱（a@b.com 的 @ 前是 a）与 URL 里的 @。
// mentionRe 匹配 @ 引用：行首或空白后的 @<token>。
// token 字符集排除常见停顿/包裹标点（中英文逗号句号分号问号叹号括号引号等），
// 避免 "@a.png，分析" 这类中文标点紧贴引用的场景被贪婪吞成 "a.png，分析"
// 导致引用静默失效；. - _ / 等文件名字符保留。
var mentionRe = regexp.MustCompile(`(^|\s)@([^\s@，。、；：！？,;!?()（）【】{}“”"'’]+)`)

// maxImageBytes 是 @ 引用的单张图片大小上限。
// base64 编码后数据约膨胀 +33%，设限防止单图撑爆上下文与请求体。
const maxImageBytes = 5 << 20 // 5 MB

// maxTotalImageBytes 是单条消息图片总量的上限（默认 4 张上限图），
// 防止一次输入引用多张图片导致请求体过大。
const maxTotalImageBytes = 4 * maxImageBytes // 20 MB

// trailingPunct 是 @ token 允许剥离的尾部标点集合（罕见兜底标点，如省略号）。
// 正则已排除绝大多数停顿/包裹标点，此处兜底不在此集合内的字面标点；
// 剥离成功的引用其整段（含尾部标点后的空白）随引用删除。
const trailingPunct = "……，。、；：,.!?;:!)]}】」』\"'）＞》”"

// parseImageMentions 解析输入文本中的图片 @ 引用，返回剥离引用后的文本与图片列表。
//
// 消费规则（只消费图片，其余一律不管，交由模型自行判断）：
//   - 命中真实图片文件（magic bytes 判定，见 detectImageMediaType）→ 剥离该引用、
//     返回 ImageRef（绝对路径 + MIME 类型；base64 数据不在此生成，
//     由 buildHistory 的 hydrateImages 按统一路径重读组装）；
//   - 文本文件 / 目录 / 不存在 / 打不开的路径 → 不消费：原样保留在文本中，
//     模型可自行决定（如调用 read_file 读取），不做任何拒绝；
//   - 被判定为图片但读取失败 / 超过大小上限 → 返回错误（调用方中止本轮，
//     不写会话——用户明确指向的图片必须可读，静默会误导模型）。
//
// 同一文件被多次引用时去重：token 全部剥离，图片只附加一次。
func parseImageMentions(cwd, input string) (string, []session.ImageRef, error) {
	matches := mentionRe.FindAllStringSubmatchIndex(input, -1)
	if len(matches) == 0 {
		return input, nil, nil
	}
	var images []session.ImageRef
	seen := map[string]bool{} // 按绝对路径去重
	var totalSize int64       // 已引用图片总字节数（含去重）
	type deletion struct{ s, e int }
	var dels []deletion
	for _, m := range matches {
		token := trimTrailingPunct(input[m[4]:m[5]]) // 剥离尾随标点（如 "@a.png。"）
		if token == "" {
			continue
		}
		path, err := expandMentionPath(cwd, token)
		if err != nil {
			continue // 路径展开失败（如无 home 目录）：不消费
		}
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue // 不存在/目录：不消费
		}
		if st.Size() > maxImageBytes {
			return input, nil, fmt.Errorf("Image file too large (%d MB exceeds the %d MB limit): %s", st.Size()>>20, maxImageBytes>>20, path)
		}
		mt, err := peekImageMediaType(path)
		if err != nil {
			continue // 打不开（权限等）：不消费
		}
		if mt == "" {
			continue // 非图片：不消费
		}
		if !seen[path] {
			seen[path] = true
			totalSize += st.Size()
			if totalSize > maxTotalImageBytes {
				return input, nil, fmt.Errorf("Total image size too large (%d MB exceeds the %d MB limit)", totalSize>>20, maxTotalImageBytes>>20)
			}
			images = append(images, session.ImageRef{Path: path, MediaType: mt})
		}
		// 整段删除（含前导空白或行首），从后往前执行保持索引有效
		dels = append(dels, deletion{m[0], m[1]})
	}
	if len(dels) == 0 {
		return input, images, nil
	}
	var b strings.Builder
	last := len(input)
	for i := len(dels) - 1; i >= 0; i-- {
		b.WriteString(input[dels[i].e:last])
		last = dels[i].s
	}
	b.WriteString(input[:last])
	return b.String(), images, nil
}

// trimTrailingPunct 剥离 @ token 尾部的常见标点（如 "@a.png。" → "a.png"）。
// 只剥离不出现在文件名中的标点字符，对路径本身无影响；
// 全部为标点时返回空串（调用方跳过）。
func trimTrailingPunct(token string) string {
	return strings.TrimRight(token, trailingPunct)
}

// expandMentionPath 把 @ token 展开为绝对路径：支持 ~ / ~/ 前缀（用户主目录）
// 与相对路径（相对 cwd）。~user/... 形式不展开（按相对路径处理，怪名罕见）。
func expandMentionPath(cwd, token string) (string, error) {
	p := token
	switch {
	case p == "~":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = home
	case strings.HasPrefix(p, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Abs(p)
}

// peekImageMediaType 读文件头按 magic bytes 判定图片类型并返回 MIME，
// 非图片返回空串。只读 16 字节，不做全量 IO。
func peekImageMediaType(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	header := make([]byte, 16)
	n, _ := io.ReadFull(f, header) // 短文件返回 EOF，header[:n] 仍有效
	return detectImageMediaType(header[:n]), nil
}

// detectImageMediaType 按 magic bytes 识别常见图片格式；不支持的类型返回空串。
func detectImageMediaType(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png"
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) >= 2 && b[0] == 'B' && b[1] == 'M':
		return "image/bmp"
	}
	return ""
}

// hydrateImages 组装消息中的图片 base64 数据：Data 为空的图片（会话恢复的
// 历史消息与当轮新注入的消息统一走此路径）重读文件生成 data URI。
//
// 重读失败（文件被删/改动/超限）时丢弃该图片并在消息文本末尾追加说明
// （用户可见，模型据此知道图片已失效），避免发送非法 part 或让模型误以为图仍存在。
// 不修改落盘记录：buildHistory 每次从会话记录重建，说明不会重复累积。
func hydrateImages(msgs []llm.Message) []llm.Message {
	for i := range msgs {
		m := &msgs[i]
		if m.Role != llm.RoleUser || len(m.Images) == 0 {
			continue
		}
		var kept []llm.Image
		var failed []string
		var totalSize int64
		for _, img := range m.Images {
			if img.Data != "" {
				// 已有 Data 的图片（当轮注入已组装）不计入总量——
				// 其体积在 parseImageMentions 当轮已受 maxTotalImageBytes 约束
				kept = append(kept, img)
				continue
			}
			// 恢复路径的总量约束：与 parseImageMentions 的当轮约束一致，
			// 防止历史多图每轮全量 base64 进上下文撑爆请求体。
			if st, err := os.Stat(img.Path); err == nil && totalSize+st.Size() > maxTotalImageBytes {
				failed = append(failed, filepath.Base(img.Path)+" (total image size limit exceeded)")
				continue
			} else if err == nil {
				totalSize += st.Size()
			}
			data, err := loadImageDataURI(img.Path, img.MediaType)
			if err != nil {
				// 只带文件名进上下文，避免把本机绝对路径泄露给模型
				failed = append(failed, filepath.Base(img.Path))
				continue
			}
			img.Data = data
			kept = append(kept, img)
		}
		m.Images = kept
		if len(failed) > 0 {
			m.Content += "\n[Attached image could not be reloaded: " + strings.Join(failed, ", ") + "]"
		}
	}
	return msgs
}

// loadImageDataURI 读取图片文件并编码为 base64 data URI
// （"data:<media_type>;base64,..."），供 provider 的 PartImage 直传。
func loadImageDataURI(path, mediaType string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Size() > maxImageBytes {
		return "", fmt.Errorf("file too large (%d MB, limit %d MB)", st.Size()>>20, maxImageBytes>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}
