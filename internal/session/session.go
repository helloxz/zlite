package session

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/helloxz/zlite/internal/llm"
)

// metaTitleEvent 是标题落盘的 meta 事件名（首条用户消息时写入）。
const metaTitleEvent = "title"

// metaSummaryEvent 是压缩摘要落盘的 meta 事件名（/compress 成功时写入）。
// 摘要不进入 History（不参与模型消息序列与轮次统计），仅在组装请求时
// 作为头部上下文注入；重启恢复由 open() 读取本事件重建。
const metaSummaryEvent = "context_summary"

// maxTitleRunes 是会话标题最大字符数（按 rune 计数，避免截断中文）。
const maxTitleRunes = 30

// Session 表示一个打开的会话（对应 ~/.zlite/sessions/<cwd-hash>/<id>.jsonl）。
type Session struct {
	ID        string
	Path      string
	Mode      string
	Model     string
	Provider  string
	Title     string // 会话标题（首条用户消息截取，持久化在 meta 记录中）
	CreatedAt string // 会话创建时间（RFC3339，取自 jsonl 首行；meta 缓存用）

	file    *os.File
	History []Record // 恢复后内存中的记录（不含 session 首行）

	// titleSet 标记标题已确定（区别于 Title=="" 的未设置状态）：
	// 首条消息为纯空白时标题为空串，但仍视为已确定，避免重复写 meta。
	titleSet bool

	// Summary 是压缩摘要（/compress 产物，存于 meta 记录，不在 History 中）。
	// SummarySet 标记已压缩：每会话最多压缩 1 次，置位后拒绝再次压缩。
	Summary    string
	SummarySet bool

	// meta 派生缓存（列表用，见 meta.go）：metaPath 是 <path>.jsonl.meta；
	// metaMessages 是内存中的 TypeMessage 计数，Append 时同步刷新并落盘。
	metaPath     string
	metaMessages int
}

// Append 追加一行记录：写文件（立即落盘）并同步到 History 与 meta 缓存。
func (s *Session) Append(r Record) error {
	if r.Ts == "" {
		r.Ts = time.Now().Format(time.RFC3339)
	}
	line, err := r.encode()
	if err != nil {
		return err
	}
	if _, err := s.file.Write(line); err != nil {
		return fmt.Errorf("写入会话失败: %w", err)
	}
	switch r.Type {
	case TypeMessage, TypeToolCall, TypeToolResult:
		s.History = append(s.History, r)
	}
	if r.Type == TypeMessage {
		s.metaMessages++
	}
	// 刷新 meta 缓存（列表用）。meta 是派生缓存，写失败静默不阻塞主路径，
	// 一致性由 open 打开会话时全量重建收敛。
	s.syncMeta(time.Now().Format(time.RFC3339Nano))
	return nil
}

// syncMeta 将当前内存状态写入 meta 缓存文件（<Path>.jsonl.meta）。
// 缓存语义：失败静默；updatedAt 为排序用的最后活跃时间（RFC3339Nano）。
func (s *Session) syncMeta(updatedAt string) {
	if s.metaPath == "" {
		return
	}
	m := &Meta{
		Version:   metaVersion,
		ID:        s.ID,
		Title:     s.Title,
		Model:     s.Model,
		Provider:  s.Provider,
		Mode:      s.Mode,
		CreatedAt: s.CreatedAt,
		UpdatedAt: updatedAt,
		Messages:  s.metaMessages,
	}
	_ = writeMeta(s.metaPath, m)
}

// AppendUser 记录用户消息。
// 首条用户消息时自动生成会话标题（截取内容前段）并以 meta 记录落盘；
// meta 写入成功后才更新内存状态，失败时调用方重试不会丢标题。
func (s *Session) AppendUser(content string) error {
	if !s.titleSet {
		title := extractTitle(content)
		if err := s.AppendMeta(metaTitleEvent, title); err != nil {
			return err
		}
		s.Title = title
		s.titleSet = true
	}
	return s.Append(Record{Type: TypeMessage, Role: "user", Content: content})
}

// extractTitle 从首条用户消息提取会话标题：去除首尾空白后取前
// maxTitleRunes 个字符，超出部分以省略号结尾。
func extractTitle(content string) string {
	s := strings.TrimSpace(content)
	runes := []rune(s)
	if len(runes) <= maxTitleRunes {
		return s
	}
	return string(runes[:maxTitleRunes]) + "…"
}

// AppendAssistant 记录助手回复（含 token 用量与思考内容）。
// reasoning 为思维链（模型 reasoning_content 增量拼接），仅落盘供展示/回放，
// 不参与模型上下文（ToMessages 不包含）。
func (s *Session) AppendAssistant(content, reasoning string, u *llm.Usage) error {
	var usage *Usage
	if u != nil {
		usage = &Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.TotalTokens}
	}
	return s.Append(Record{Type: TypeMessage, Role: "assistant", Content: content, Reasoning: reasoning, Usage: usage})
}

// AppendToolCall 记录工具调用。
func (s *Session) AppendToolCall(callID, name string, input map[string]any) error {
	return s.Append(Record{Type: TypeToolCall, CallID: callID, Name: name, Input: input})
}

// AppendToolResult 记录工具结果。
// 失败时（err=true）内容前缀 "error: "，与模型收到的格式一致，保证恢复语义。
func (s *Session) AppendToolResult(callID, name, output string, err bool, dur time.Duration) error {
	if err {
		output = "error: " + output
	}
	return s.Append(Record{
		Type: TypeToolResult, CallID: callID, Name: name,
		Output: output, Error: err, DurationMS: dur.Milliseconds(),
	})
}

// AppendMeta 记录元事件（模式切换等，不参与模型上下文）。
func (s *Session) AppendMeta(event, value string) error {
	return s.Append(Record{Type: TypeMeta, Event: event, Value: value})
}

// AppendSummary 落盘压缩摘要（/compress 成功时调用）。
// 与 AppendUser 的标题模式一致：meta 写入成功后才更新内存状态，
// 失败时调用方重试不会出现内存与落盘不一致。
func (s *Session) AppendSummary(content string) error {
	if err := s.AppendMeta(metaSummaryEvent, content); err != nil {
		return err
	}
	s.Summary = content
	s.SummarySet = true
	return nil
}

// ToMessages 把历史转换为模型消息序列。
//
// 配对规则（design.md §2.2）：
//   - assistant 文本与其后紧随的 tool_call 记录合并为一条带 ToolCalls 的 assistant 消息
//   - tool_result 记录转为 tool 消息（按 call_id 关联）
func (s *Session) ToMessages() []llm.Message {
	var out []llm.Message
	for _, r := range s.History {
		switch r.Type {
		case TypeMessage:
			if r.Role == "user" {
				out = append(out, llm.Message{Role: llm.RoleUser, Content: r.Content})
			} else if r.Role == "assistant" {
				out = append(out, llm.Message{Role: llm.RoleAssistant, Content: r.Content})
			}
		case TypeToolCall:
			// 合并到紧随其后的 assistant 消息
			if len(out) == 0 || out[len(out)-1].Role != llm.RoleAssistant {
				out = append(out, llm.Message{Role: llm.RoleAssistant})
			}
			m := &out[len(out)-1]
			m.ToolCalls = append(m.ToolCalls, llm.ToolCall{ID: r.CallID, Name: r.Name, Input: r.Input})
		case TypeToolResult:
			out = append(out, llm.Message{
				Role: llm.RoleTool, Content: r.Output,
				ToolCallID: r.CallID, ToolName: r.Name,
			})
		}
	}
	return out
}

// Close 关闭会话文件。
func (s *Session) Close() error {
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

// newSessionID 生成会话 ID：时间戳 + 3 位随机后缀。
func newSessionID(t time.Time) string {
	return t.Format("20060102-1504") + "-" + randSuffix()
}

const suffixChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// randSuffix 生成 3 位随机后缀（基于时间纳秒，够用即可）。
func randSuffix() string {
	n := time.Now().UnixNano()
	var b strings.Builder
	for i := 0; i < 3; i++ {
		b.WriteByte(suffixChars[n%int64(len(suffixChars))])
		n /= int64(len(suffixChars))
	}
	return b.String()
}

// readAll 读取 jsonl 文件全部记录（跳过空行）。
func readAll(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var recs []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		r, err := decode([]byte(line))
		if err != nil {
			return nil, err
		}
		recs = append(recs, r)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return recs, nil
}
