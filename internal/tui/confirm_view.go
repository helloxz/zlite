package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/helloxz/zlite/internal/agent"
)

// confirmDialog 是危险操作的确认弹窗（覆盖层，模态）。
// 三选项单行排布：Allow / Deny / Cancel；←/→ 循环选择，Enter 确认，Esc = Cancel。
// Cancel 语义：仅拒绝本次操作（回传 Denied，界面显示 Cancelled 区分）。
// 弹窗是瞬态 UI 不落盘；聊天区保留 "Approve? <summary>" 提示与决策结果行
// （会话恢复后仍可见确认历史）。
type confirmDialog struct {
	summary string
	sel     int // 0=Allow, 1=Deny, 2=Cancel
	ch      chan agent.ApprovalDecision
}

// confirmLabels 选项显示文本。
var confirmLabels = []string{"Allow", "Deny", "Cancel"}

// confirmMove 移动选择（循环 wrap：Allow ↔ Deny ↔ Cancel）。
func (t *TUI) confirmMove(dy int) {
	c := t.confirm
	if c == nil {
		return
	}
	c.sel = (c.sel + dy + len(confirmLabels)) % len(confirmLabels)
}

// confirmConfirm 确认选中项并关闭弹窗：决策经 channel 回传阻塞中的
// Approver.Request，结果行按选项着色（Approved 绿 / Denied 红 / Cancelled 黄）。
func (t *TUI) confirmConfirm() {
	c := t.confirm
	if c == nil {
		return
	}
	t.confirm = nil
	switch c.sel {
	case 1:
		c.ch <- agent.Denied
		t.chat.appendSystem(colorize("Denied", ansiRed))
	case 2:
		// Cancel 语义 = 拒绝本次操作（与 Deny 同值，界面区分显示）
		c.ch <- agent.Denied
		t.chat.appendSystem(colorize("Cancelled", ansiYellow))
	default:
		c.ch <- agent.Approved
		t.chat.appendSystem(colorize("Approved", ansiGreen))
	}
}

// confirmCancel 取消（Esc）：等同选择 Cancel。
func (t *TUI) confirmCancel() {
	c := t.confirm
	if c == nil {
		return
	}
	c.sel = 2
	t.confirmConfirm()
}

// renderConfirmBox 渲染确认弹窗边框盒（多行字符串）。
// 结构：标题行（黄色）+ 摘要（按宽度硬断行，超出高度截断）+ 选项行
// （选中项 Cyan 底黑字高亮，与 picker 选中风格一致）。
func (t *TUI) renderConfirmBox(w, h int) string {
	c := t.confirm
	if c == nil {
		return ""
	}

	// 选项行：每项等宽（默认 10，"[ Allow ] " 形），项间 2 空格；
	// 窄窗口下收缩格宽（最小 8），避免选项行溢出框体。
	optW := 10
	optRow := ""
	for {
		optRow = renderConfirmOptRow(c, optW)
		if ansi.StringWidth(optRow)+4 <= w-2 || optW <= 8 {
			break
		}
		optW--
	}

	// 内容区宽度：max(选项行, 摘要行) + 两侧留白，限制 [34, w-2]
	contentW := ansi.StringWidth(optRow) + 4
	if contentW < 34 {
		contentW = 34
	}
	if contentW > w-2 {
		contentW = w - 2
	}

	// 摘要按内容区宽度硬断行（CJK 安全）；行数上限受屏幕高约束
	bodyW := maxInt(1, contentW-2)
	summaryLines := wrapByWidth(c.summary, bodyW)
	maxLines := maxInt(1, h-8) // 高度上限 h-4 减去标题/选项/边框 4 行
	if len(summaryLines) > maxLines {
		summaryLines = summaryLines[:maxLines]
	}

	// 高度：标题 1 + 摘要 + 选项 1 + 边框 2，限制 [6, h-4]
	height := len(summaryLines) + 4
	if height > h-4 {
		height = h - 4
	}
	if height < 6 {
		height = 6
	}

	var b strings.Builder
	b.WriteString(colorize(" Approve? ", ansiYellow))
	b.WriteString("\n")
	for _, l := range summaryLines {
		b.WriteString(" " + l)
		b.WriteString("\n")
	}
	b.WriteString(" " + optRow)

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder(), true, true, true, true).
		Width(contentW).
		BorderForeground(lipgloss.Color("240")).
		Render(strings.TrimSuffix(b.String(), "\n"))
	return box
}

// renderConfirmOptRow 渲染选项行：每项等宽 optW（内容右补空格对齐），
// 选中项整块高亮（Cyan 底黑字），项间 2 空格。
func renderConfirmOptRow(c *confirmDialog, optW int) string {
	optCells := make([]string, len(confirmLabels))
	for i, l := range confirmLabels {
		item := "[ " + l + " ]"
		// 收缩格宽时 item 可能已超 optW：pad 非负 clamp（防 Repeat 负数 panic）
		if pad := optW - ansi.StringWidth(item); pad > 0 {
			item += strings.Repeat(" ", pad)
		}
		if i == c.sel {
			sel := lipgloss.NewStyle().
				Background(lipgloss.Color("39")).
				Foreground(lipgloss.Color("0"))
			optCells[i] = sel.Render(item)
		} else {
			optCells[i] = item
		}
	}
	return strings.Join(optCells, "  ")
}

// wrapByWidth 按显示宽度硬断行（CJK 按 2 列计）。
// 摘要为纯文本（工具摘要无 ANSI），不做转义处理。
func wrapByWidth(s string, w int) []string {
	var lines []string
	if w <= 0 {
		return []string{s}
	}
	var cur strings.Builder
	curW := 0
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, cur.String())
			cur.Reset()
			curW = 0
			continue
		}
		rw := ansi.StringWidth(string(r))
		if curW+rw > w && curW > 0 {
			lines = append(lines, cur.String())
			cur.Reset()
			curW = 0
		}
		cur.WriteRune(r)
		curW += rw
	}
	if curW > 0 {
		lines = append(lines, cur.String())
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return lines
}
