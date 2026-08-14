package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// picker 是通用列表选择弹窗（/switch 模型、/thinking 思考强度、/sessions
// 会话共用）的覆盖层状态。nil 表示未打开（模态）。
//
// 渲染方式：View() 把聊天区按弹窗位置切分，中间插入边框盒（按行切分，
// 不触碰行内 ANSI 序列）。键位处理在 model.handleKey 中优先于聊天快捷键。
type picker struct {
	title  string
	labels []string
	values []string
	sel    int
	// scroll 是弹窗内部滚动偏移（条目超过可视高度时使用；跟随选中行）。
	scroll int
	// onPick 在确认时调用（此时弹窗已关闭）；返回错误会显示在聊天区。
	onPick func(value string) error
}

// openPicker 打开列表选择弹窗（消息循环线程调用）。
// labels 为每行显示文本，values 为确认时回调收到的选择值（模型名/会话 ID）；
// initial 为初始选中行；onPick 在确认时调用（此时弹窗已关闭）。
func (t *TUI) openPicker(title string, labels, values []string, initial int, onPick func(value string) error) error {
	if len(labels) == 0 || len(labels) != len(values) {
		t.chat.appendSystem(colorize("Nothing to pick", ansiRed))
		return nil
	}
	if initial < 0 {
		initial = 0
	}
	if initial >= len(labels) {
		initial = len(labels) - 1
	}
	t.picker = &picker{
		title:  title,
		labels: labels,
		values: values,
		sel:    initial,
		onPick: onPick,
	}
	return nil
}

// pickerOpen 返回弹窗是否打开（模态）。
func (t *TUI) pickerOpen() bool {
	return t.picker != nil
}

// pickerMove 移动选中行（clamp，不循环；与 gocui 版一致）。
func (t *TUI) pickerMove(dy int) {
	p := t.picker
	if p == nil {
		return
	}
	if p.sel+dy < 0 {
		p.sel = 0
	} else if p.sel+dy >= len(p.labels) {
		p.sel = len(p.labels) - 1
	} else {
		p.sel += dy
	}
	// 内部滚动跟随选中行
	vis := t.pickerVisibleLines()
	if p.sel < p.scroll {
		p.scroll = p.sel
	}
	if p.sel >= p.scroll+vis {
		p.scroll = p.sel - vis + 1
	}
}

// pickerConfirm 确认选中项并关闭弹窗（调用 openPicker 注入的 onPick）。
func (t *TUI) pickerConfirm() {
	p := t.picker
	if p == nil {
		return
	}
	value := p.values[p.sel]
	t.picker = nil
	if p.onPick != nil {
		if err := p.onPick(value); err != nil {
			t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		}
	}
}

// pickerCancel 取消选择（Esc）。
func (t *TUI) pickerCancel() {
	t.picker = nil
}

// pickerVisibleLines 返回弹窗内可同时显示的选项行数（高度受屏幕限制）。
func (t *TUI) pickerVisibleLines() int {
	// 高度上限：屏幕高 - 4（避开状态栏与输入区）；盒内：边框 2 + 标题 1
	return maxInt(1, t.screenH-4-3)
}

// renderPickerBox 渲染弹窗边框盒（多行字符串，宽度/高度按内容与屏幕计算）。
// 结构：标题行（dim）+ 分隔 + 选项行（选中行 Cyan 底黑字高亮）。
func (t *TUI) renderPickerBox(w, h int) string {
	p := t.picker
	if p == nil {
		return ""
	}

	// 宽度：最长标签显示宽度 + 留白，限制在 [24, w-2]
	width := 0
	for _, l := range p.labels {
		if lw := ansi.StringWidth(l); lw > width {
			width = lw
		}
	}
	width += 4
	if width < 24 {
		width = 24
	}
	if width > w-2 {
		width = w - 2
	}

	// 高度：标题 1 + 选项 + 边框 2，限制在 [4, h-4]
	vis := t.pickerVisibleLines()
	height := vis + 3
	if height > h-4 {
		height = h - 4
	}
	if height < 4 {
		height = 4
	}

	// 选项可视窗口
	optVis := height - 3
	if optVis < 1 {
		optVis = 1
	}
	start := minInt(p.scroll, maxInt(0, len(p.labels)-optVis))
	end := minInt(start+optVis, len(p.labels))

	var b strings.Builder
	b.WriteString(colorize(" "+p.title+" ", ansiDim))
	b.WriteString("\n")
	for i := start; i < end; i++ {
		label := p.labels[i]
		if i == p.sel {
			// 选中行高亮：Cyan 底黑字（对应 gocui 版 SelBgColor/SelFgColor）
			sel := lipgloss.NewStyle().
				Background(lipgloss.Color("39")).
				Foreground(lipgloss.Color("0")).
				Padding(0, 1)
			b.WriteString(sel.Render(label))
		} else {
			b.WriteString(" " + label)
		}
		b.WriteString("\n")
	}

	// 边框盒（RoundedBorder；CJK locale 对齐已由 M0 断言验证）
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder(), true, true, true, true).
		Width(width).
		BorderForeground(lipgloss.Color("240")).
		Render(strings.TrimSuffix(b.String(), "\n"))
	return box
}

// maxInt/minInt 是局部小工具（避免为两个调用引入泛型依赖）。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
