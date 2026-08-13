package session

import (
	"encoding/json"
	"fmt"
)

// 记录类型常量。
const (
	TypeSession    = "session"
	TypeMessage    = "message"
	TypeToolCall   = "tool_call"
	TypeToolResult = "tool_result"
	TypeMeta       = "meta"
)

// Record 是 jsonl 中的一行记录。
// 所有类型共用一个结构，由 Type 字段区分；各类型只使用相关字段（见 design.md §2.1）。
type Record struct {
	Type string `json:"type"`

	// message 记录
	ID      string `json:"id,omitempty"`
	Role    string `json:"role,omitempty"` // user | assistant
	Content string `json:"content,omitempty"`
	Usage   *Usage `json:"usage,omitempty"` // assistant 消息的 token 用量

	// tool_call / tool_result 记录（按 CallID 配对）
	CallID     string         `json:"call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
	Output     string         `json:"output,omitempty"`
	Error      bool           `json:"error,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`

	// session 首行记录
	Cwd       string `json:"cwd,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Version   string `json:"version,omitempty"`

	// meta 记录
	Event string `json:"event,omitempty"`
	Value string `json:"value,omitempty"`

	// 通用时间戳（RFC3339）
	Ts string `json:"ts,omitempty"`
}

// Usage 是 token 用量（jsonl 中的小写 key 形式）。
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// encode 把记录编码为单行 JSON。
func (r Record) encode() ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("编码记录失败: %w", err)
	}
	return append(b, '\n'), nil
}

// decode 解析单行 JSON 为记录。
func decode(line []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(line, &r); err != nil {
		return Record{}, fmt.Errorf("解析会话记录失败: %w", err)
	}
	return r, nil
}
