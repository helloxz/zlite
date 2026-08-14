package tui

import (
	"fmt"

	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/llm"
)

// statusView 是底部状态栏的纯状态模型：模式 / 模型 / token 用量 / 忙碌 /
// 思考强度。渲染由 model.View() 经 title() 生成（边框盒，位置在输入区上方）。
type statusView struct {
	mode  agent.Mode
	model string
	usage llm.Usage // 保留（DoneEvent 更新），但不再在状态栏展示
	busy  bool
	// thinking 是当前思考强度显示值（默认 auto）。
	thinking string
}

func newStatusView(model string) *statusView {
	return &statusView{mode: agent.ModePlan, model: model, thinking: "auto"}
}

func (s *statusView) setMode(m agent.Mode) {
	s.mode = m
}

func (s *statusView) setUsage(u llm.Usage) {
	s.usage = u
}

func (s *statusView) setBusy(b bool) {
	s.busy = b
}

func (s *statusView) setModel(m string) {
	s.model = m
}

func (s *statusView) setThinking(t string) {
	s.thinking = t
}

// title 生成状态栏文本（纯文本，不嵌 ANSI——边框绘制不解析转义）。
// 注意：不再展示 token 用量（usage 字段保留，仅隐藏）；busy 显示在末尾，
// 前置 | 分隔避免与命令提示混在一起。模型引用（provider_name/model_name）
// 可能较长，截断避免挤压 thinking 显示。
func (s *statusView) title() string {
	busy := ""
	if s.busy {
		busy = " | *busy*"
	}
	model := truncateDisplay(s.model, 28)
	return fmt.Sprintf(" [%s] %s | thinking: %s | /help for commands%s ",
		string(s.mode), model, s.thinking, busy)
}
