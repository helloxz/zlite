package acp

import (
	"context"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/helloxz/zlite/internal/agent"
)

// startPump 启动会话的事件翻译 goroutine：消费 agent 事件流并转成
// ACP session update 推送客户端。会话关闭（stop 关闭）时退出。
func (a *Agent) startPump(st *sessionState) {
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()
		for {
			select {
			case <-st.stop:
				return
			case ev := <-st.ag.Events():
				a.translate(st, ev)
			}
		}
	}()
}

// translate 把一条 agent 事件映射为 ACP session update。
// 连接断开后发送失败仅忽略（会话本身已不可达）。
func (a *Agent) translate(st *sessionState, ev agent.Event) {
	if a.conn == nil {
		return
	}
	ctx := context.Background()
	switch e := ev.(type) {
	case agent.TextDeltaEvent:
		// 流式文本增量
		_ = a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
			SessionId: st.sid,
			Update:    acpsdk.UpdateAgentMessageText(e.Text),
		})
	case agent.ToolCallEvent:
		// 工具调用开始（status=in_progress）
		_ = a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
			SessionId: st.sid,
			Update: acpsdk.StartToolCall(
				acpsdk.ToolCallId(e.CallID),
				toolTitle(e.Name),
				acpsdk.WithStartKind(toolKind(e.Name)),
				acpsdk.WithStartStatus(acpsdk.ToolCallStatusInProgress),
				acpsdk.WithStartRawInput(e.Input),
			),
		})
	case agent.ToolResultEvent:
		// 工具执行结果（completed / failed）
		status := acpsdk.ToolCallStatusCompleted
		if e.Error {
			status = acpsdk.ToolCallStatusFailed
		}
		_ = a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
			SessionId: st.sid,
			Update: acpsdk.UpdateToolCall(
				acpsdk.ToolCallId(e.CallID),
				acpsdk.WithUpdateStatus(status),
				acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(e.Output))}),
				acpsdk.WithUpdateRawOutput(map[string]any{"content": e.Output}),
			),
		})
	case agent.ModeChangeEvent:
		// 模式切换（session/set_mode 或加载会话时触发）
		_ = a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
			SessionId: st.sid,
			Update: acpsdk.SessionUpdate{CurrentModeUpdate: &acpsdk.SessionCurrentModeUpdate{
				CurrentModeId: acpsdk.SessionModeId(e.Mode),
				SessionUpdate: "current_mode_update",
			}},
		})
	case agent.ReasoningDeltaEvent:
		// 思考内容增量（模型 reasoning_content）→ agent_thought_chunk，
		// 客户端可流式展示思维链
		_ = a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
			SessionId: st.sid,
			Update:    acpsdk.UpdateAgentThoughtText(e.Text),
		})
	case agent.TitleUpdatedEvent:
		// 标题异步更新（首条消息 AI 生成），推送 session_info_update
		title := e.Title
		_ = a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{
			SessionId: st.sid,
			Update: acpsdk.SessionUpdate{
				SessionInfoUpdate: &acpsdk.SessionSessionInfoUpdate{
					Title:         &title,
					SessionUpdate: "session_info_update",
				},
			},
		})
	}
	// ThinkingStartEvent / DoneEvent / TextDoneEvent 不单独映射：
	// 思考开始状态由 thought chunk 本身表达；结束以 Prompt 响应（stopReason）表达。
}

// toolTitle 生成工具调用的展示标题（当前直接用工具名）。
func toolTitle(name string) string {
	return name
}

// toolKind 把 zlite 工具名映射为 ACP ToolKind（client 据此选择图标与 UI 表现）。
func toolKind(name string) acpsdk.ToolKind {
	switch name {
	case "read_file":
		return acpsdk.ToolKindRead
	case "grep", "glob":
		return acpsdk.ToolKindSearch
	case "run_command":
		return acpsdk.ToolKindExecute
	case "write_file", "edit_file":
		return acpsdk.ToolKindEdit
	case "delete":
		return acpsdk.ToolKindDelete
	case "web_fetch":
		return acpsdk.ToolKindFetch
	case "read_skill":
		return acpsdk.ToolKindRead
	default:
		return acpsdk.ToolKindOther
	}
}
