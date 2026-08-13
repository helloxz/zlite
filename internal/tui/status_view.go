package tui

import (
	"fmt"

	"github.com/awesome-gocui/gocui"
	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/llm"
)

// statusView 管理底部输入框边框上的 Title 状态栏：
// 模式 / 模型 / token 用量 / 忙碌状态（gocui 的 setRune 固定 +1 边框偏移，
// 1 行无边框视图内容会画到屏幕外，因此状态信息放在带边框输入框的 Title 上）。
type statusView struct {
	view  *gocui.View // 输入框 view（Title 显示在其顶边框）
	mode  agent.Mode
	model string
	usage llm.Usage
	busy  bool
}

func newStatusView(v *gocui.View, model string) *statusView {
	s := &statusView{view: v, mode: agent.ModePlan, model: model}
	v.TitleColor = gocui.ColorCyan
	return s
}

func (s *statusView) setMode(m agent.Mode) {
	s.mode = m
	s.render()
}

func (s *statusView) setUsage(u llm.Usage) {
	s.usage = u
	s.render()
}

func (s *statusView) setBusy(b bool) {
	s.busy = b
	s.render()
}

func (s *statusView) setModel(m string) {
	s.model = m
	s.render()
}

// title 生成状态栏文本（纯文本，不嵌 ANSI——边框绘制不解析转义）。
func (s *statusView) title() string {
	busy := ""
	if s.busy {
		busy = " *busy*"
	}
	return fmt.Sprintf(" [%s] %s%s | up %d down %d | /help for commands ",
		string(s.mode), s.model, busy, s.usage.InputTokens, s.usage.OutputTokens)
}

// render 更新 Title 并强制重绘（Write(nil) 置 tainted，空写不改变内容）。
func (s *statusView) render() {
	if s.view == nil {
		return
	}
	s.view.Title = s.title()
	s.view.Write(nil)
}
