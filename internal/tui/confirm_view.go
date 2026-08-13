package tui

// confirm_view 是二期（M2/P1）的内联确认层：Agent 发出确认请求时弹出覆盖层，
// 展示 diff 预览 / 完整命令，等待用户 y/n 决策。
//
// 一期（M1）的确认由 agent.NewApprover 提供 autoApprover/nilApprover 顶替
// （tools 注册表一期无需要确认的工具），本文件仅占位，二期实现：
//
//	type tuiApprover struct {
//	    t *TUI
//	}
//
//	func (a *tuiApprover) Request(ctx context.Context, req agent.ApprovalRequest) (agent.ApprovalDecision, error) {
//	    // 1. 在 chat 区上方弹出覆盖层视图 confirm
//	    // 2. 展示 req.Summary（diff 预览或完整命令）
//	    // 3. 等待 y（批准）/ n（拒绝）/ Esc（拒绝并终止本轮）
//	    // 4. 返回决策；ctx 取消视为拒绝
//	}
//
// 二期在 cmd/zlite 组装处用 tuiApprover 替换 agent.NewApprover(...)。
