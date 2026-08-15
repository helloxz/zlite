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

// ThinkingStartEvent 是模型开始输出思考内容（reasoning 增量出现）时发出。
// 仅广播一次；模型未返回思考内容时不发此事件。
// UI 据此把生成状态从 [processing...] 切换为 [thinking...]。
type ThinkingStartEvent struct{}

// ReasoningDeltaEvent 是思考内容增量（模型 reasoning_content 逐段产生）。
// 内容透传给 UI/ACP 展示；TUI 当前不消费（保持 [thinking...] 状态），
// ACP 翻译层转为 agent_thought_chunk 推给客户端。
type ReasoningDeltaEvent struct {
	Text string
}

// DoneEvent 是一轮对话结束（含 token 用量）。
type DoneEvent struct{ Usage llm.Usage }

// ApprovalRequest 是确认请求（直接传给 Approver，不经事件通道）。
// Input 是工具调用参数（ACP 权限请求展示用；TUI 确认不消费该字段）。
type ApprovalRequest struct {
	CallID  string
	Tool    string
	Summary string
	Input   map[string]any
}

// SystemNoticeEvent 是系统级提示（如 MCP 连接警告），
// TUI 展示为 system 消息；ACP 翻译器不匹配则静默忽略。
type SystemNoticeEvent struct {
	Text string
}

func (SystemNoticeEvent) isEvent() {}

func (TextDeltaEvent) isEvent()      {}
func (TextDoneEvent) isEvent()       {}
func (ToolCallEvent) isEvent()       {}
func (ToolResultEvent) isEvent()     {}
func (ModeChangeEvent) isEvent()     {}
func (ThinkingStartEvent) isEvent()  {}
func (ReasoningDeltaEvent) isEvent() {}
func (DoneEvent) isEvent()           {}
