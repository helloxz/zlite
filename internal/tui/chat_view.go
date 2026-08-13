package tui

import (
	"fmt"
	"strings"

	"github.com/awesome-gocui/gocui"
	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/llm"
)

// entry 是聊天区的一条展示记录（text 存原始文本，渲染时着色）。
// thinking 是生成状态标记（仅 assistant 消息用，由对话生命周期 + 思维链事件驱动）：
//
//	"processing" = 本轮生成中（提交后立即显示 [processing...]）；
//	"thinking" = 后端返回了思维链（切换为 [thinking...]）；
//	"done" = 本轮生成结束（显示 [done]）；"" = 无状态或历史消息。
//	标记只存在于显示层，不落盘、历史恢复时不出现。
type entry struct {
	kind     string // user | assistant | tool | system
	text     string
	thinking string
}

// chatView 是消息历史视图。
type chatView struct {
	view    *gocui.View
	entries []entry
	md      mdRenderer
	// autoScroll=true 时跟随底部（新消息自动贴底显示）；
	// 用户上翻看历史后置 false，滚回底部时自动恢复。
	autoScroll bool
}

func newChatView(v *gocui.View) *chatView {
	v.Autoscroll = true
	v.Wrap = true
	return &chatView{view: v, autoScroll: true}
}

// appendUser 追加用户消息（每轮对话前插入一条弱化分隔线，标明回合边界）。
// 用户主动发言视为关注最新内容：若此前正在上翻看历史，先恢复自动滚动
// （回到底部）再追加。历史恢复（loadHistory）复用本方法，分隔线结构一致。
func (c *chatView) appendUser(text string) {
	c.scrollToBottom()
	c.entries = append(c.entries, entry{kind: "divider"})
	c.entries = append(c.entries, entry{kind: "user", text: text})
	c.render()
}

// appendAssistantDelta 追加助手流式增量（自动归并到当前助手消息）。
// 生成标记不固化：渲染时动态显示在内容上方（[processing...] 或 [thinking...] 换行），
// 直到整轮生成结束由 finishProcessing 置为 done。
func (c *chatView) appendAssistantDelta(delta string) {
	if n := len(c.entries); n == 0 || c.entries[n-1].kind != "assistant" {
		c.entries = append(c.entries, entry{kind: "assistant"})
	}
	c.entries[len(c.entries)-1].text += delta
	c.render()
}

// startProcessing 标记当前助手消息进入生成中状态（用户提交后同步调用，
// 立即显示 [processing...]）。已在生成中则忽略（防重复调用）。
func (c *chatView) startProcessing() {
	if n := len(c.entries); n > 0 && c.entries[n-1].kind == "assistant" && c.entries[n-1].thinking == "processing" {
		return
	}
	c.entries = append(c.entries, entry{kind: "assistant", thinking: "processing"})
	c.render()
}

// confirmThinking 把生成中状态切换为思考中（ThinkingStartEvent：
// 后端返回了思维链）。仅 processing → thinking 单向切换；其余状态忽略。
func (c *chatView) confirmThinking() {
	if n := len(c.entries); n > 0 && c.entries[n-1].kind == "assistant" && c.entries[n-1].thinking == "processing" {
		c.entries[n-1].thinking = "thinking"
		c.render()
	}
}

// finishProcessing 把生成状态标记为已结束（整轮生成完成后调用）。
// 无论是否有思维链统一置 done；无进行中标记时忽略（防御）。
func (c *chatView) finishProcessing() {
	for i := len(c.entries) - 1; i >= 0; i-- {
		if c.entries[i].kind == "assistant" && c.entries[i].thinking != "" && c.entries[i].thinking != "done" {
			c.entries[i].thinking = "done"
			break
		}
	}
	c.render()
}

// appendToolCall 追加工具调用行（待完成状态）。
func (c *chatView) appendToolCall(e agent.ToolCallEvent) {
	summary := compactInput(e.Input)
	c.entries = append(c.entries, entry{kind: "tool", text: "  [tool] " + e.Name + summary + " ..."})
	c.render()
}

// finishToolCall 更新最后一条工具行状态（[ok] / [fail]）。
func (c *chatView) finishToolCall(e agent.ToolResultEvent) {
	for i := len(c.entries) - 1; i >= 0; i-- {
		if c.entries[i].kind == "tool" && strings.HasSuffix(c.entries[i].text, " ...") {
			mark := colorize("[ok]", ansiGreen)
			if e.Error {
				mark = colorize("[fail]", ansiRed)
			}
			c.entries[i].text = strings.TrimSuffix(c.entries[i].text, " ...") + " " + mark
			break
		}
	}
	c.render()
}

// appendSystem 追加系统提示（斜杠命令反馈等）。
func (c *chatView) appendSystem(text string) {
	c.entries = append(c.entries, entry{kind: "system", text: text})
	c.render()
}

// reset 清空聊天区（/new 新会话用）。
func (c *chatView) reset() {
	c.entries = nil
	c.autoScroll = true // 新会话回到跟随底部
	c.render()
}

// loadHistory 用模型消息历史整段重建聊天区（/sessions 切换会话后调用）。
// 工具调用以摘要行展示（历史中不保留 [ok]/[fail] 状态，与实时流区分）。
func (c *chatView) loadHistory(msgs []llm.Message) {
	c.reset()
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			c.appendUser(m.Content)
		case llm.RoleAssistant:
			c.entries = append(c.entries, entry{kind: "assistant", text: m.Content})
			for _, tc := range m.ToolCalls {
				c.entries = append(c.entries, entry{
					kind: "tool",
					text: "  [tool] " + tc.Name + compactInput(tc.Input),
				})
			}
		case llm.RoleTool:
			// 工具结果已由 assistant 的工具行代表，不再单独展示
		}
	}
	c.render()
}

// render 重绘整个聊天区。
// 手动滚动（autoScroll=false）时保留当前 Origin：Clear() 会重置原点，
// 重写后恢复，避免新消息渲染把用户拉回顶部。
func (c *chatView) render() {
	_, oy := c.view.Origin() // 记录手动滚动位置（Clear 前）
	c.view.Autoscroll = c.autoScroll
	c.view.Clear()
	for _, e := range c.entries {
		switch e.kind {
		case "divider":
			// 回合分隔线：弱化灰色，铺满内容区宽度
			_, w := c.view.Size()
			if w < 4 {
				w = 4
			}
			fmt.Fprintln(c.view, colorize(strings.Repeat("-", w), ansiGray))
		case "user":
			fmt.Fprintln(c.view, colorize("You: "+e.text, ansiCyan))
			// 用户消息与 AI 答复之间留一个空行，视觉上分隔输入与输出
			fmt.Fprintln(c.view)
		case "assistant":
			prefix := "Zlite: "
			switch e.thinking {
			case "processing":
				// 生成中：标记后换行，流式内容从下一行出现
				prefix += colorize("[processing...]", ansiYellow) + "\n"
			case "thinking":
				// 思考中（后端返回了思维链）：标记后换行，流式内容从下一行出现
				prefix += colorize("[thinking...]", ansiYellow) + "\n"
			case "done":
				// 生成结束：统一 [done]，输出内容在下方
				prefix += colorize("[done]", ansiGreen) + "\n"
			}
			fmt.Fprintln(c.view, colorize(prefix, ansiCyan)+c.md.Render(e.text))
		case "tool":
			fmt.Fprintln(c.view, colorize(e.text, ansiYellow))
		case "system":
			fmt.Fprintln(c.view, colorize(e.text, ansiCyan))
		}
	}
	if !c.autoScroll {
		c.view.SetOrigin(0, c.clampOy(oy))
	}
}

// clampOy 把滚动位置限制在有效范围 [0, 总行数-可视高度]（与 gocui 的
// Autoscroll 底部算法一致），内容减少时防止越界滚出空白。
func (c *chatView) clampOy(oy int) int {
	_, maxY := c.view.Size()
	if maxY <= 0 {
		return 0
	}
	maxOy := c.view.ViewLinesHeight() - maxY - 1
	if maxOy < 0 {
		maxOy = 0
	}
	if oy < 0 {
		oy = 0
	}
	if oy > maxOy {
		oy = maxOy
	}
	return oy
}

// scrollBy 按 dy 行滚动聊天区（负数上翻看历史，正数下翻）。
// 滚到最底部时恢复自动跟随（autoScroll=true），此后新消息继续贴底。
func (c *chatView) scrollBy(dy int) {
	if dy == 0 {
		return
	}
	_, maxY := c.view.Size()
	if maxY <= 0 {
		return
	}
	_, oy := c.view.Origin()
	oy = c.clampOy(oy + dy)
	if maxOy := c.view.ViewLinesHeight() - maxY - 1; oy >= maxOy {
		// 到达（或滚过）底部：恢复自动滚动
		c.autoScroll = true
		c.view.Autoscroll = true
	} else {
		c.autoScroll = false
		c.view.Autoscroll = false
	}
	c.view.SetOrigin(0, oy)
}

// scrollToTop 直接滚动到顶部（查看最早的消息），并退出自动滚动：
// 后续新消息渲染不把用户拉走，直到手动滚回底部或发消息。
func (c *chatView) scrollToTop() {
	c.autoScroll = false
	c.view.Autoscroll = false
	c.view.SetOrigin(0, 0)
}

// scrollToBottom 恢复自动跟随底部。
func (c *chatView) scrollToBottom() {
	c.autoScroll = true
	c.view.Autoscroll = true
}

// compactInput 把工具参数压缩为单行摘要（截断过长）。
func compactInput(input map[string]any) string {
	if len(input) == 0 {
		return ""
	}
	parts := make([]string, 0, len(input))
	for k, v := range input {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	s := " " + strings.Join(parts, " ")
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return s
}
