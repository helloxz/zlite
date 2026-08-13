package agent

import "github.com/helloxz/zlite/internal/llm"

// Event 是 agent 对外发射的事件（TUI / ACP 共享契约）。
type Event interface{ isEvent() }

// TextDeltaEvent 是流式文本增量。
type TextDeltaEvent struct{ Text string }

// TextDoneEvent 是单次回复的完整文本。
type TextDoneEvent struct{ FullText string }

// ToolCallEvent 是模型请求的工具调用（工具开始执行前发出）。
type ToolCallEvent struct {
	CallID string
	Name   string
	Input  map[string]any
}

// ToolResultEvent 是工具执行结果。
type ToolResultEvent struct {
	CallID string
	Name   string
	Output string
	Error  bool
}

// ModeChangeEvent 是模式切换事件。
type ModeChangeEvent struct{ Mode Mode }

// DoneEvent 是一轮对话结束（含 token 用量）。
type DoneEvent struct{ Usage llm.Usage }

// ApprovalRequest 是确认请求（直接传给 Approver，不经事件通道）。
type ApprovalRequest struct {
	CallID  string
	Tool    string
	Summary string
}

func (TextDeltaEvent) isEvent()  {}
func (TextDoneEvent) isEvent()   {}
func (ToolCallEvent) isEvent()   {}
func (ToolResultEvent) isEvent() {}
func (ModeChangeEvent) isEvent() {}
func (DoneEvent) isEvent()       {}
