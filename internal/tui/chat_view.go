package tui

import (
	"fmt"
	"strings"

	"github.com/awesome-gocui/gocui"
	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/llm"
)

// entry 是聊天区的一条展示记录（text 存原始文本，渲染时着色）。
type entry struct {
	kind string // user | assistant | tool | system
	text string
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

// appendUser 追加用户消息。用户主动发言视为关注最新内容：若此前
// 正在上翻看历史，先恢复自动滚动（回到底部）再追加。
func (c *chatView) appendUser(text string) {
	c.scrollToBottom()
	c.entries = append(c.entries, entry{kind: "user", text: text})
	c.render()
}

// appendAssistantDelta 追加助手流式增量（自动归并到当前助手消息）。
func (c *chatView) appendAssistantDelta(delta string) {
	if n := len(c.entries); n == 0 || c.entries[n-1].kind != "assistant" {
		c.entries = append(c.entries, entry{kind: "assistant"})
	}
	c.entries[len(c.entries)-1].text += delta
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
		case "user":
			fmt.Fprintln(c.view, colorize("You: "+e.text, ansiCyan))
		case "assistant":
			fmt.Fprintln(c.view, colorize("Assistant: ", ansiCyan)+c.md.Render(e.text))
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
