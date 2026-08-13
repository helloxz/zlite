package acp

import (
	"context"
	"errors"
	"fmt"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/helloxz/zlite/internal/agent"
)

// acpApprover 是 ACP 模式的权限确认实现：把 agent 的确认请求转成
// ACP permission_request 发给客户端，同步等待客户端响应。
// 每个会话一个实例（绑定 session id）。
type acpApprover struct {
	conn *acpsdk.AgentSideConnection
	sid  acpsdk.SessionId
}

// Request 发起权限请求；ctx 取消（turn 被取消）视为拒绝。
func (ap *acpApprover) Request(ctx context.Context, req agent.ApprovalRequest) (agent.ApprovalDecision, error) {
	if ap.conn == nil {
		return agent.Denied, errors.New("acp connection not attached")
	}

	title := req.Summary
	if title == "" {
		title = req.Tool
	}
	resp, err := ap.conn.RequestPermission(ctx, acpsdk.RequestPermissionRequest{
		SessionId: ap.sid,
		ToolCall: acpsdk.ToolCallUpdate{
			ToolCallId: acpsdk.ToolCallId(req.CallID),
			Title:      acpsdk.Ptr(title),
			Kind:       acpsdk.Ptr(toolKind(req.Tool)),
			Status:     acpsdk.Ptr(acpsdk.ToolCallStatusPending),
			RawInput:   req.Input,
		},
		Options: []acpsdk.PermissionOption{
			{Kind: acpsdk.PermissionOptionKindAllowOnce, Name: "Allow this operation", OptionId: "allow"},
			{Kind: acpsdk.PermissionOptionKindRejectOnce, Name: "Reject this operation", OptionId: "reject"},
		},
	})
	if err != nil {
		if ctx.Err() != nil {
			return agent.Denied, ctx.Err()
		}
		return agent.Denied, fmt.Errorf("permission request failed: %w", err)
	}
	if resp.Outcome.Cancelled != nil {
		// 客户端取消权限请求 = 拒绝（与 reject 同语义，非错误）
		return agent.Denied, nil
	}
	if resp.Outcome.Selected != nil && resp.Outcome.Selected.OptionId == "allow" {
		return agent.Approved, nil
	}
	return agent.Denied, nil
}
