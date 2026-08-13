package tui

import (
	"fmt"
	"strings"

	"github.com/awesome-gocui/gocui"
	"github.com/helloxz/zlite/internal/agent"
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
}

func newChatView(v *gocui.View) *chatView {
	v.Autoscroll = true
	v.Wrap = true
	return &chatView{view: v}
}

// appendUser 追加用户消息。
func (c *chatView) appendUser(text string) {
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
	c.render()
}

// render 重绘整个聊天区。
func (c *chatView) render() {
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
