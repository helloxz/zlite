package tui

import (
	"errors"
	"fmt"

	"github.com/awesome-gocui/gocui"
)

// modelPickerViewName 是 /switch 模型选择弹窗的视图名。
//
// 弹窗 view 在 layout() 中预创建并常驻，运行期只切换 Visible/坐标，
// 不增删 g.views：gocui v1.1.0 的 loaderTick goroutine 每 50ms 遍历
// g.Views()（无锁读 g.views），主循环线程 SetView/DeleteView 写 g.views
// 会触发数据竞争（race detector 实测）；SetView 对已存在 view 仅更新
// 坐标字段，不写 slice，可安全用于移动弹窗。
const modelPickerViewName = "modelpicker"

// openModelPicker 弹出模型选择列表（/switch 无参数时调用，主循环线程）：
// 居中覆盖层视图，↑/↓ 选择、Enter 确认、Esc 取消。
// 打开时默认定位到当前模型；仅当配置了至少一个模型时才弹出。
func (t *TUI) openModelPicker() error {
	if len(t.models) == 0 {
		t.chat.appendSystem(colorize("No models configured (providers[0].models)", ansiRed))
		return nil
	}
	g := t.g
	maxX, maxY := g.Size()

	// 尺寸：宽度按最长模型名 + 留白，高度按模型数（每行一个）
	width := 0
	for _, m := range t.models {
		if len(m) > width {
			width = len(m)
		}
	}
	width += 4
	if width < 24 {
		width = 24
	}
	if width > maxX-2 {
		width = maxX - 2
	}
	height := len(t.models) + 2 // 边框占 2 行
	if height > maxY-4 {
		height = maxY - 4
	}
	x0 := (maxX - width) / 2
	y0 := (maxY - height) / 2

	// 更新位置并显示（view 已存在：仅改坐标字段，不写 g.views）
	v, err := g.SetView(modelPickerViewName, x0, y0, x0+width, y0+height, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	v.Visible = true

	// 重新绑定按键（先清理残留；keybindings 仅主循环线程访问）
	g.DeleteKeybindings(modelPickerViewName)
	g.SetKeybinding(modelPickerViewName, gocui.KeyArrowDown, gocui.ModNone, t.pickerMoveDown)
	g.SetKeybinding(modelPickerViewName, gocui.KeyArrowUp, gocui.ModNone, t.pickerMoveUp)
	g.SetKeybinding(modelPickerViewName, gocui.KeyEnter, gocui.ModNone, t.pickerConfirm)
	g.SetKeybinding(modelPickerViewName, gocui.KeyEsc, gocui.ModNone, t.pickerCancel)

	// 默认定位到当前模型
	t.pickerSel = 0
	for i, m := range t.models {
		if m == t.model {
			t.pickerSel = i
			break
		}
	}
	t.renderModelPicker(v)

	_, err = g.SetCurrentView(modelPickerViewName)
	return err
}

// renderModelPicker 重绘弹窗内容并把光标移到选中行（Highlight 据此高亮）。
func (t *TUI) renderModelPicker(v *gocui.View) {
	v.Clear()
	for _, m := range t.models {
		fmt.Fprintf(v, "  %s\n", m)
	}
	_ = v.SetCursor(0, t.pickerSel)
}

// closeModelPicker 关闭弹窗：隐藏 view、清 keybinding、恢复输入焦点。
// view 保留（Visible=false），下次打开只改坐标，不增删 g.views。
func (t *TUI) closeModelPicker(g *gocui.Gui) {
	g.DeleteKeybindings(modelPickerViewName)
	if v, err := g.View(modelPickerViewName); err == nil {
		v.Visible = false
	}
	_, _ = g.SetCurrentView(inputViewName)
}

// pickerMoveDown 下移选中行。
func (t *TUI) pickerMoveDown(g *gocui.Gui, v *gocui.View) error {
	if t.pickerSel < len(t.models)-1 {
		t.pickerSel++
	}
	t.renderModelPicker(v)
	return nil
}

// pickerMoveUp 上移选中行。
func (t *TUI) pickerMoveUp(g *gocui.Gui, v *gocui.View) error {
	if t.pickerSel > 0 {
		t.pickerSel--
	}
	t.renderModelPicker(v)
	return nil
}

// pickerConfirm 确认选中模型并关闭弹窗。
func (t *TUI) pickerConfirm(g *gocui.Gui, v *gocui.View) error {
	name := t.models[t.pickerSel]
	t.closeModelPicker(g)
	return t.switchToModel(name)
}

// pickerCancel 取消选择（Esc）。
func (t *TUI) pickerCancel(g *gocui.Gui, v *gocui.View) error {
	t.closeModelPicker(g)
	return nil
}
