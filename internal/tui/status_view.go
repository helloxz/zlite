package tui

import (
	"fmt"
	"strings"

	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/llm"
)

// ansiPathGray 是状态栏右侧项目路径的灰色前景（与左侧信息区分）。
const ansiPathGray = "\x1b[38;5;244m"

// statusView 是底部状态栏的纯状态模型：模式 / 模型 / token 用量 / 忙碌 /
// 思考强度 / 工作目录。渲染由 model.View() 经 title(w) 生成（边框盒）。
type statusView struct {
	mode  agent.Mode
	model string
	usage llm.Usage // 保留（DoneEvent 更新），但不再在状态栏展示
	busy  bool
	// thinking 是当前思考强度显示值（默认 auto）。
	thinking string
	// cwd 是项目工作目录（展示在状态栏最右侧，灰色，左右对齐）。
	cwd string
}

func newStatusView(model, cwd string) *statusView {
	return &statusView{mode: agent.ModePlan, model: model, thinking: "auto", cwd: cwd}
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

// minPathWidth 是右侧路径可展示的最小宽度（含省略号），低于则隐藏。
const minPathWidth = 14

// title 生成状态栏文本（w 为边框内内容宽度）：左段（模式/模型/thinking/
// busy）与右段（项目路径，灰色）左右对齐，中间空格填充。
//
// 路径超宽时左侧截断保留尾部（…/last/dirs 更有辨识度）；剩余空间不足
// minPathWidth 时隐藏路径（保持与旧版一致的左段布局）。
// 注意：不再展示 token 用量（usage 字段保留，仅隐藏）；模型引用
// （provider_name/model_name）可能较长，截断避免挤压右侧路径。
func (s *statusView) title(w int) string {
	busy := ""
	if s.busy {
		busy = " | *busy*"
	}
	// 模式着色：plan 绿色 / build 红色（视觉区分；颜色序列不计宽，
	// displayWidth 会剥 ANSI，窄屏截断走 truncateDisplay 的 ANSI 感知路径）。
	modeStr := string(s.mode)
	switch s.mode {
	case agent.ModePlan:
		modeStr = colorize(modeStr, ansiBlue) // plan 标准蓝（build 保持红）
	case agent.ModeBuild:
		modeStr = colorize(modeStr, ansiRed)
	}
	model := truncateDisplay(s.model, 28)
	left := fmt.Sprintf(" [%s] %s | thinking: %s | /help for commands%s ",
		modeStr, model, s.thinking, busy)
	leftW := displayWidth(left)

	pathText := s.cwd
	pathW := displayWidth(pathText)
	// 完整展示：左右段 + 至少 2 列填充
	if pathW >= minPathWidth && leftW+pathW+2 <= w {
		return left + strings.Repeat(" ", w-leftW-pathW) + ansiPathGray + pathText + ansiReset
	}
	// 截断路径（左侧省略，保留尾部）
	pathMax := w - leftW - 3 // 留 2 列填充 + 1 列省略号余量
	if pathMax >= minPathWidth {
		trunc := truncateLeft(pathText, pathMax)
		return left + strings.Repeat(" ", w-leftW-displayWidth(trunc)) + ansiPathGray + trunc + ansiReset
	}
	// 空间不足：截断左段到内容宽（保留核心信息；/help 提示优先被截掉）。
	// 不能返回超宽字符串——lipgloss Width 对超宽内容会 wrap 成多行，
	// 破坏边框盒布局（窄屏实测）。
	if leftW > w {
		return truncateDisplay(left, w)
	}
	return left + strings.Repeat(" ", w-leftW)
}

// truncateLeft 按显示宽度从左侧截断 s，保留尾部并以 "…" 开头
// （路径展示用：目录层级中末尾部分最有辨识度）。
func truncateLeft(s string, w int) string {
	runes := []rune(s)
	if displayWidth(s) <= w {
		return s
	}
	width := 0
	for i := len(runes) - 1; i >= 0; i-- {
		rw := 1
		if runes[i] > 0x2E7F { // CJK 等宽字符计 2（与 truncateDisplay 一致）
			rw = 2
		}
		if width+rw+1 > w { // 留 1 列给省略号
			return "…" + string(runes[i+1:])
		}
		width += rw
	}
	return "…" + s
}
