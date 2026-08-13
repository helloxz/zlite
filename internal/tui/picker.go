package tui

import (
	"errors"
	"fmt"

	"github.com/awesome-gocui/gocui"
)

// pickerViewName 是列表选择弹窗（/switch 模型、/sessions 会话共用）的视图名。
//
// 弹窗 view 在 layout() 中预创建并常驻，运行期只切换 Visible/坐标，
// 不增删 g.views：gocui v1.1.0 的 loaderTick goroutine 每 50ms 遍历
// g.Views()（无锁读 g.views），主循环线程 SetView/DeleteView 写 g.views
// 会触发数据竞争（race detector 实测）；SetView 对已存在 view 仅更新
// 坐标字段，不写 slice，可安全用于移动弹窗。
const pickerViewName = "picker"

// openPicker 弹出通用列表选择弹窗（主循环线程调用）：
// 居中覆盖层视图，↑/↓ 选择、Enter 确认、Esc 取消。
// labels 为每行显示文本（含前导空格），values 为 Enter 确认时回调收到的
// 选择值（模型名 / 会话 ID 等）；initial 为初始选中行；onPick 在确认时
// 调用（此时弹窗已关闭）。
func (t *TUI) openPicker(title string, labels, values []string, initial int, onPick func(g *gocui.Gui, value string) error) error {
	if len(labels) == 0 || len(labels) != len(values) {
		t.chat.appendSystem(colorize("Nothing to pick", ansiRed))
		return nil
	}
	g := t.g
	maxX, maxY := g.Size()

	// 尺寸：宽度按最长标签的显示宽度（CJK 计 2）+ 留白，高度按条目数
	width := 0
	for _, l := range labels {
		if w := displayWidth(l); w > width {
			width = w
		}
	}
	width += 4
	if width < 24 {
		width = 24
	}
	if width > maxX-2 {
		width = maxX - 2
	}
	height := len(labels) + 2 // 边框占 2 行
	if height > maxY-4 {
		height = maxY - 4
	}
	x0 := (maxX - width) / 2
	y0 := (maxY - height) / 2

	// 更新位置并显示（view 已存在：仅改坐标字段，不写 g.views）
	v, err := g.SetView(pickerViewName, x0, y0, x0+width, y0+height, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	v.Visible = true
	v.Title = title

	// 弹窗期间隐藏终端光标：g.Cursor 全局开启时 gocui 会把光标画在
	// 选中行开头（覆盖前导空格，看起来第一行错位）；行高亮由
	// Highlight + Sel 配色渲染，不依赖光标位置。
	g.Cursor = false

	// 重新绑定按键（先清理残留；keybindings 仅主循环线程访问）
	g.DeleteKeybindings(pickerViewName)
	g.SetKeybinding(pickerViewName, gocui.KeyArrowDown, gocui.ModNone, t.pickerMoveDown)
	g.SetKeybinding(pickerViewName, gocui.KeyArrowUp, gocui.ModNone, t.pickerMoveUp)
	g.SetKeybinding(pickerViewName, gocui.KeyEnter, gocui.ModNone, t.pickerConfirm)
	g.SetKeybinding(pickerViewName, gocui.KeyEsc, gocui.ModNone, t.pickerCancel)

	t.pickerLabels = labels
	t.pickerValues = values
	t.pickerOnPick = onPick
	if initial < 0 {
		initial = 0
	}
	if initial >= len(labels) {
		initial = len(labels) - 1
	}
	t.pickerSel = initial
	t.renderPicker(v)

	if _, err = g.SetCurrentView(pickerViewName); err != nil {
		g.Cursor = true // 聚焦失败时恢复光标，避免界面残留隐藏状态
		return err
	}
	return nil
}

// renderPicker 重绘弹窗内容并把光标移到选中行（Highlight 据此高亮）。
func (t *TUI) renderPicker(v *gocui.View) {
	v.Clear()
	for _, l := range t.pickerLabels {
		fmt.Fprintf(v, "%s\n", l)
	}
	_ = v.SetCursor(0, t.pickerSel)
}

// closePicker 关闭弹窗：隐藏 view、清 keybinding、恢复输入焦点与光标。
// view 保留（Visible=false），下次打开只改坐标，不增删 g.views。
func (t *TUI) closePicker(g *gocui.Gui) {
	g.DeleteKeybindings(pickerViewName)
	if v, err := g.View(pickerViewName); err == nil {
		v.Visible = false
	}
	g.Cursor = true // 恢复输入框光标（openPicker 中已隐藏）
	_, _ = g.SetCurrentView(inputViewName)
}

// pickerMoveDown 下移选中行。
func (t *TUI) pickerMoveDown(g *gocui.Gui, v *gocui.View) error {
	if t.pickerSel < len(t.pickerLabels)-1 {
		t.pickerSel++
	}
	t.renderPicker(v)
	return nil
}

// pickerMoveUp 上移选中行。
func (t *TUI) pickerMoveUp(g *gocui.Gui, v *gocui.View) error {
	if t.pickerSel > 0 {
		t.pickerSel--
	}
	t.renderPicker(v)
	return nil
}

// pickerConfirm 确认选中项并关闭弹窗（调用 openPicker 注入的 onPick）。
func (t *TUI) pickerConfirm(g *gocui.Gui, v *gocui.View) error {
	value := t.pickerValues[t.pickerSel]
	t.closePicker(g)
	if t.pickerOnPick != nil {
		return t.pickerOnPick(g, value)
	}
	return nil
}

// pickerCancel 取消选择（Esc）。
func (t *TUI) pickerCancel(g *gocui.Gui, v *gocui.View) error {
	t.closePicker(g)
	return nil
}
