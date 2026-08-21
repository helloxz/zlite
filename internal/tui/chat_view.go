package tui

import (
	"fmt"
	"strings"

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
//
// callID 是工具调用的调用 ID（仅 tool 行用，用于并行工具调用时按 ID 匹配
// 完成状态；历史恢复的工具行无 callID）。
type entry struct {
	kind     string // user | assistant | tool | system
	text     string
	thinking string
	callID   string
}

// chatView 是消息历史的纯状态模型：只维护 entries 与滚动跟随状态，
// 渲染统一由 model.refreshChat() 经 renderString(w) 生成字符串并写入 viewport。
// （bubbletea 消息循环天然串行，原 gocui 版的 uiTasks 队列已删除。）
type chatView struct {
	entries []entry
	md      mdRenderer
	// autoScroll=true 时跟随底部（新消息自动贴底显示）；
	// 用户上翻看历史后置 false，滚回底部时自动恢复。
	autoScroll bool
}

func newChatView() *chatView {
	return &chatView{autoScroll: true}
}

// appendUser 追加用户消息（每轮对话前插入一条空行分隔，标明回合边界）。
// 用户主动发言视为关注最新内容：若此前正在上翻看历史，先恢复自动滚动
// （回到底部）再追加。历史恢复（loadHistory）复用本方法，分隔线结构一致。
func (c *chatView) appendUser(text string) {
	c.scrollToBottom()
	c.entries = append(c.entries, entry{kind: "divider"})
	c.entries = append(c.entries, entry{kind: "user", text: text})
}

// appendAssistantDelta 追加助手流式增量（自动归并到当前助手消息）。
// 生成标记不固化：渲染时动态显示在内容上方（[processing...] 或 [thinking...] 换行），
// 直到整轮生成结束由 finishProcessing 置为 done。
func (c *chatView) appendAssistantDelta(delta string) {
	if n := len(c.entries); n == 0 || c.entries[n-1].kind != "assistant" {
		c.entries = append(c.entries, entry{kind: "assistant"})
	}
	c.entries[len(c.entries)-1].text += delta
}

// startProcessing 标记当前助手消息进入生成中状态（用户提交后同步调用，
// 立即显示 [processing...]）。已在生成中则忽略（防重复调用）。
func (c *chatView) startProcessing() {
	if n := len(c.entries); n > 0 && c.entries[n-1].kind == "assistant" && c.entries[n-1].thinking == "processing" {
		return
	}
	c.entries = append(c.entries, entry{kind: "assistant", thinking: "processing"})
}

// confirmThinking 把生成中状态切换为思考中（ThinkingStartEvent：
// 后端返回了思维链）。仅 processing → thinking 单向切换；其余状态忽略。
func (c *chatView) confirmThinking() {
	if n := len(c.entries); n > 0 && c.entries[n-1].kind == "assistant" && c.entries[n-1].thinking == "processing" {
		c.entries[n-1].thinking = "thinking"
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
}

// appendToolCall 追加工具调用行（待完成状态）。
func (c *chatView) appendToolCall(e agent.ToolCallEvent) {
	summary := compactInput(e.Input)
	c.entries = append(c.entries, entry{kind: "tool", text: "  [tool] " + e.Name + summary + " ...", callID: e.CallID})
}

// finishToolCall 按调用 ID 更新对应工具行状态（[ok] / [fail]）。
// 并行工具调用按 CallID 精确匹配，避免完成顺序不同导致状态挂错行。
func (c *chatView) finishToolCall(e agent.ToolResultEvent) {
	for i := len(c.entries) - 1; i >= 0; i-- {
		if c.entries[i].kind == "tool" && c.entries[i].callID == e.CallID {
			mark := colorize("[ok]", ansiGreen)
			if e.Error {
				mark = colorize("[fail]", ansiRed)
			}
			c.entries[i].text = strings.TrimSuffix(c.entries[i].text, " ...") + " " + mark
			break
		}
	}
}

// appendSystem 追加系统提示（斜杠命令反馈等）。
func (c *chatView) appendSystem(text string) {
	c.entries = append(c.entries, entry{kind: "system", text: text})
}

// reset 清空聊天区（/new 新会话用）。
func (c *chatView) reset() {
	c.entries = nil
	c.autoScroll = true // 新会话回到跟随底部
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
}

// renderString 生成聊天区完整渲染字符串（头带按 w 补空格）。
// 手动滚动（autoScroll=false）时由调用方保留当前滚动位置：
// viewport.SetContent 会 clamp 越界偏移，无需像 gocui 版手动恢复 Origin。
// 注意：mdRenderer 为流式增量设计（inCodeBlock 跨片段），但本方法每次重绘全量历史，
// 必须用全新渲染器而非复用 c.md——否则未闭合代码块在下一帧起始状态错误会导致首段颜色翻转闪烁。
func (c *chatView) renderString(w int) string {
	var b strings.Builder
	var md mdRenderer // 每帧全新，避免跨帧状态污染
	for _, e := range c.entries {
		switch e.kind {
		case "divider":
			// 回合分隔：空行。角色头带已经能分出你/助手，不再画整宽虚线。
			b.WriteString("\n")
		case "user":
			b.WriteString(paintLine(" You: "+e.text, ansiBarUser, w))
			// 用户消息与 AI 答复之间留一个空行，视觉上分隔输入与输出
			b.WriteString("\n\n")
		case "assistant":
			head := " Zlite: "
			mark, markFg := "", ""
			switch e.thinking {
			case "processing":
				// 生成中：标记后换行，流式内容从下一行出现
				mark, markFg = "[processing...]", ansiFgProc
			case "thinking":
				// 思考中（后端返回了思维链）：标记后换行，流式内容从下一行出现
				mark, markFg = "[thinking...]", ansiFgThink
			case "done":
				// 生成结束：统一 [done]，输出内容在下方
				mark, markFg = "[done]", ansiFgDone
			}
			b.WriteString(paintBar(head, mark, markFg, ansiBarZlite, w))
			b.WriteString("\n")
			if e.text != "" {
				b.WriteString(md.Render(e.text))
				b.WriteString("\n")
			}
		case "tool":
			rest := strings.TrimPrefix(e.text, "  [tool]")
			b.WriteString(paintLine(" [tool]", ansiBarTool, 0) + colorize(rest, ansiYellow))
			b.WriteString("\n")
		case "system":
			// 系统提示降为 dim，和用户青色头带区分；已预着色的 Error/Approved 仍走内层 SGR。
			b.WriteString(colorize(e.text, ansiDim))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// scrollToTop 直接滚动到顶部（查看最早的消息），并退出自动滚动：
// 后续新消息渲染不把用户拉走，直到手动滚回底部或发消息。
func (c *chatView) scrollToTop() {
	c.autoScroll = false
}

// scrollToBottom 恢复自动跟随底部。
func (c *chatView) scrollToBottom() {
	c.autoScroll = true
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
