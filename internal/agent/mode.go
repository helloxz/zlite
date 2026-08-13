package agent

import (
	"context"
	"fmt"
)

// Mode 是 agent 运行模式（与 config.ModePlan/ModeBuild 字符串一致）。
type Mode string

const (
	ModePlan  Mode = "plan"
	ModeBuild Mode = "build"
)

// ParseMode 解析模式字符串。
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModePlan, ModeBuild:
		return Mode(s), nil
	}
	return "", fmt.Errorf("未知模式: %q（仅支持 plan / build）", s)
}

// ApprovalDecision 是确认结果。
type ApprovalDecision int

const (
	Approved ApprovalDecision = iota
	Denied
)

// Approver 处理工具执行前的确认请求。
// 实现与 UI/传输层解耦：TUI 内联确认、auto_approve、ACP permission 均为其实现。
type Approver interface {
	// Request 请求确认；ctx 取消视为拒绝。
	Request(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)
}

// autoApprover 自动批准（agent.auto_approve = true 的信任模式）。
type autoApprover struct{}

func (autoApprover) Request(context.Context, ApprovalRequest) (ApprovalDecision, error) {
	return Approved, nil
}

// nilApprover 拒绝一切（UI 确认实现未接入前的安全兜底）。
type nilApprover struct{}

func (nilApprover) Request(context.Context, ApprovalRequest) (ApprovalDecision, error) {
	return Denied, nil
}

// NewApprover 按配置构造一期默认 Approver。
// autoApprove=true 时自动批准；否则拒绝一切（T7 接入 TUI 确认后替换）。
func NewApprover(autoApprove bool) Approver {
	if autoApprove {
		return autoApprover{}
	}
	return nilApprover{}
}
