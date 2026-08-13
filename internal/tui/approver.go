package tui

import (
	"context"
	"errors"

	"github.com/helloxz/zlite/internal/agent"
)

// Approver 是 build 模式下危险操作（危险 shell 命令）的 TUI 确认实现：
// 在聊天区展示确认请求，用户输入 y/n 决策。
//
// 写文件工具（write_file/edit_file/delete）按用户决策直接执行，不经过确认；
// 仅 run_command 的危险命令（黑名单/危险模式）需要确认。
type Approver struct {
	t *TUI
}

// Attach 绑定 TUI 实例（agent 先于 TUI 创建，组装完成后调用）。
func (a *Approver) Attach(t *TUI) {
	a.t = t
}

// Request 展示确认提示并等待用户决策；ctx 取消视为拒绝。
func (a *Approver) Request(ctx context.Context, req agent.ApprovalRequest) (agent.ApprovalDecision, error) {
	if a.t == nil {
		return agent.Denied, errors.New("TUI not attached")
	}

	ch := make(chan agent.ApprovalDecision, 1)
	shown := make(chan struct{})
	a.t.ui(func() {
		a.t.chat.appendSystem(colorize("Approve? "+req.Summary+"  [y/n] ", ansiYellow))
		a.t.approvalCh = ch
		close(shown)
	})

	select {
	case <-shown:
	case <-ctx.Done():
		return agent.Denied, ctx.Err()
	}

	select {
	case d := <-ch:
		return d, nil
	case <-ctx.Done():
		a.t.ui(func() { a.t.approvalCh = nil })
		return agent.Denied, ctx.Err()
	}
}
