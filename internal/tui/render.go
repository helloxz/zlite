package tui

import (
	"regexp"
	"strings"
)

// ANSI 颜色码（gocui 的 escape.go 解析 SGR 序列）。
// 头带用 256 色深灰底（Run 走 Output256）；8 色码走 outputNormal 回退。
const (
	ansiReset    = "\x1b[0m"
	ansiGreen    = "\x1b[32m"                     // 代码块 / [ok]
	ansiYellow   = "\x1b[33m"                     // 工具参数 / 状态强调
	ansiRed      = "\x1b[31m"                     // 错误
	ansiDim      = "\x1b[2m"                      // 系统提示
	ansiBarUser  = "\x1b[38;5;116m\x1b[48;5;237m" // 浅青字 + 深灰底
	ansiBarZlite = "\x1b[38;5;250m\x1b[48;5;236m" // 浅灰字 + 更深灰底
	ansiBarTool  = "\x1b[38;5;187m\x1b[48;5;236m" // 浅khaki 字 + 深灰底
)

// colorize 用指定 ANSI 颜色码包裹文本。
func colorize(s, code string) string {
	if s == "" {
		return ""
	}
	return code + s + ansiReset
}

// paintLine 把可见文本铺满内容区宽度并套上 SGR，形成角色头带。
// 超宽不截断，交给 view.Wrap；w<=0 时只着色不补空格。
func paintLine(s, style string, w int) string {
	if s == "" {
		return ""
	}
	if w > 0 {
		if n := displayWidth(s); n < w {
			s += strings.Repeat(" ", w-n)
		}
	}
	return style + s + ansiReset
}

// mdRenderer 做轻量渲染（design.md 决策 D5）：
//   - ``` 代码块整体绿色（流式增量时维护跨片段状态）
//   - `行内代码` 绿色（仅完整片段）
//   - 其余纯文本，不引入 markdown 库
type mdRenderer struct {
	// inCodeBlock 记录上一片段结束时是否处于代码块内（流式增量状态）。
	inCodeBlock bool
}

var inlineCodeRe = regexp.MustCompile("`[^`\n]+`")

// Render 渲染一段文本（含增量拼接场景）。
func (r *mdRenderer) Render(text string) string {
	if !strings.Contains(text, "```") && !r.inCodeBlock {
		// 无代码块：行内代码着色
		return inlineCodeRe.ReplaceAllStringFunc(text, func(s string) string {
			return colorize(s, ansiGreen)
		})
	}

	var b strings.Builder
	rest := text
	for {
		idx := strings.Index(rest, "```")
		if idx < 0 {
			b.WriteString(r.wrap(rest))
			break
		}
		b.WriteString(r.wrap(rest[:idx]))
		rest = rest[idx+3:]
		r.inCodeBlock = !r.inCodeBlock
		if r.inCodeBlock {
			// 代码块起始：```lang 后的语言标识不显示
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 && nl <= 32 && !strings.Contains(rest[:nl], "```") {
				rest = rest[nl+1:]
			}
		}
	}
	return b.String()
}

// wrap 按当前代码块状态着色。
func (r *mdRenderer) wrap(s string) string {
	if s == "" {
		return ""
	}
	if r.inCodeBlock {
		return colorize(s, ansiGreen)
	}
	// 代码块外：行内代码着色
	return inlineCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		return colorize(m, ansiGreen)
	})
}
