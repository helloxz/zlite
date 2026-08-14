package tui

import (
	"errors"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/helloxz/zlite/internal/agent"
)

// ---- 消息类型（消息循环内部契约） ----

// agentEventMsg 包装 agent 事件流中的一条事件。
type agentEventMsg struct{ ev agent.Event }

// agentDoneMsg 是 runAgent/runInit 后台完成回调（对应 gocui 版的 t.ui(...)）。
type agentDoneMsg struct{ err error }

// setupStartMsg 触发首次配置引导（Init 阶段投递，等价 gocui 版
// MainLoop 首循环后的 UpdateAsync 投递）。
type setupStartMsg struct{}

// agentResubscribeMsg 请求重新订阅 agent 事件流（SetAgent 热重载后用）。
type agentResubscribeMsg struct{}

// approvalRequestMsg 是 Approver 的确认请求（程序 Send 进入消息循环）。
type approvalRequestMsg struct {
	summary string
	ch      chan agent.ApprovalDecision
}

// approvalCancelMsg 是 Approver 因 ctx 取消而放弃等待（清 pendingApproval）。
type approvalCancelMsg struct{ ch chan agent.ApprovalDecision }

// refreshMsg 请求重绘（外部注入 API 在消息循环外修改共享状态后触发）。
type refreshMsg struct{}

// ---- model：bubbletea 的消息循环状态 ----

// model 持有 *TUI 引用：共享状态（chat/status/approvalCh 等）都在 TUI 上，
// 消息只负责触发处理；tea 在每次 Update 后自动重绘，外部注入 API 的改动
// 在下一次渲染自然生效。
type model struct {
	t      *TUI
	vp     *viewport.Model
	ta     textarea.Model
	width  int
	height int
}

func newModel(t *TUI) model {
	ta := textarea.New()
	ta.Placeholder = "Type a message... (Enter=send, Shift+Enter=newline, Tab=mode, Ctrl+C=quit)"
	ta.ShowLineNumbers = false
	ta.Focus()
	return model{t: t, ta: ta}
}

// waitAgentEvent 订阅 agent 事件流：每次处理完一条后由 Update 重新返回，
// 形成循环订阅；t.ctx.Done 用于退出时解除阻塞（防 goroutine 泄漏）。
func waitAgentEvent(events <-chan agent.Event, done <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			return agentEventMsg{ev: ev}
		case <-done:
			return nil
		}
	}
}

func (m model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.t.agent != nil {
		cmds = append(cmds, waitAgentEvent(m.t.agent.Events(), m.t.ctx.Done()))
	}
	if m.t.setupNeeded {
		cmds = append(cmds, func() tea.Msg { return setupStartMsg{} })
	}
	return tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	t := m.t
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleResize(msg)

	case setupStartMsg:
		t.startSetup()
		m.refreshChat()
		return m, nil

	case agentEventMsg:
		m.handleAgentEvent(msg.ev)
		m.refreshChat()
		// 继续订阅（events 通道由 agent 生命周期管理）
		if t.agent != nil {
			return m, waitAgentEvent(t.agent.Events(), t.ctx.Done())
		}
		return m, nil

	case agentResubscribeMsg:
		if t.agent != nil {
			return m, waitAgentEvent(t.agent.Events(), t.ctx.Done())
		}
		return m, nil

	case agentDoneMsg:
		t.status.setBusy(false)
		t.chat.finishProcessing() // 整轮生成结束：统一置 [done]
		if msg.err != nil {
			t.chat.appendSystem(colorize("Error: "+msg.err.Error(), ansiRed))
		}
		m.refreshChat()
		return m, nil

	case refreshMsg:
		m.refreshChat()
		return m, nil

	case approvalRequestMsg:
		t.chat.appendSystem(colorize("Approve? "+msg.summary+"  [y/n] ", ansiYellow))
		t.approvalCh = msg.ch
		m.refreshChat()
		return m, nil

	case approvalCancelMsg:
		if t.approvalCh == msg.ch {
			t.approvalCh = nil
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// 其余消息（普通按键输入等）交给 textarea
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// handleResize 响应窗口尺寸变化：聊天区占主体，底部状态栏（3 行边框盒）
// + 输入区（3 行），留 1 行余量避免总行数超出屏幕被终端截断（M0 实测）。
// vp 用指针字段：viewport.Model 是值类型，若内嵌值拷贝，值接收者方法内
// 的 SetContent/PageUp 等 *Model 方法只会修改临时拷贝（实测 bug，已修复）。
func (m model) handleResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width, m.height = msg.Width, msg.Height
	m.t.screenH = msg.Height // 弹窗布局用
	v := viewport.New(msg.Width, msg.Height-7)
	m.vp = &v
	m.ta.SetWidth(msg.Width - 2)
	m.ta.SetHeight(3)
	m.refreshChat()
	return m, nil
}

// refreshChat 把聊天区内容同步到 viewport 并处理自动跟随。
func (m model) refreshChat() {
	if m.vp == nil {
		return // 尚未收到 WindowSizeMsg
	}
	m.vp.SetContent(m.t.chat.renderString(m.vp.Width))
	if m.t.chat.autoScroll {
		m.vp.GotoBottom()
	}
}

// handleKey 处理快捷键（优先级高于 textarea 默认行为）。
// 弹窗打开时是模态：只响应弹窗键，其余按键（滚动/输入等）忽略。
func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	t := m.t

	if t.pickerOpen() {
		switch msg.String() {
		case keyQuit:
			// Ctrl+C 全局退出优先于模态弹窗（与 gocui 版全局绑定一致）
			return m, tea.Quit
		case keyDown:
			t.pickerMove(1)
		case keyUp:
			t.pickerMove(-1)
		case keySubmit:
			t.pickerConfirm() // onPick 可能 appendSystem，需刷新 viewport
			m.refreshChat()
		case keyCancel:
			t.pickerCancel()
		}
		return m, nil
	}

	switch msg.String() {
	case keyQuit:
		return m, tea.Quit
	case keyToggleMode:
		t.toggleMode()
		m.refreshChat()
		return m, nil
	case keySubmit:
		return m.submit()
	case keyNewline:
		m.ta.InsertString("\n")
		return m, nil
	case keySessions:
		// Ctrl+L：/sessions 等效
		t.handleSessions()
		m.refreshChat()
		return m, nil
	case keyNewChat:
		// Ctrl+N：/new 等效（newChat 内部已显示错误）
		t.newChat()
		m.refreshChat()
		return m, nil
	case keyThinking:
		// Ctrl+T：/thinking 等效
		t.handleThinking("")
		m.refreshChat()
		return m, nil
	case keySwitch:
		// Shift+Tab：/switch（无参数）等效
		t.handleSwitch("")
		m.refreshChat()
		return m, nil
	case keyPageUp:
		if m.vp != nil {
			m.vp.PageUp()
			m.syncAutoScroll()
		}
		return m, nil
	case keyPageDown:
		if m.vp != nil {
			m.vp.PageDown()
			m.syncAutoScroll()
		}
		return m, nil
	case keyTop:
		if m.vp != nil {
			t.chat.scrollToTop()
			m.vp.GotoTop()
		}
		return m, nil
	case keyBottom:
		if m.vp != nil {
			t.chat.scrollToBottom()
			m.vp.GotoBottom()
		}
		return m, nil
	}
	// 其余按键交给 textarea（字符输入、方向键、退格等）
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// syncAutoScroll 滚动操作后同步跟随状态：滚回底部恢复跟随，离开底部取消。
// TODO(M2): 弹窗打开时滚动应忽略（gocui 版 chatScroll 的 modal 检查）。
func (m model) syncAutoScroll() {
	if m.vp.AtBottom() {
		m.t.chat.scrollToBottom()
	} else {
		m.t.chat.autoScroll = false
	}
}

// submit 提交输入：先处理待确认操作，再处理斜杠命令，否则交给 agent。
func (m model) submit() (tea.Model, tea.Cmd) {
	t := m.t
	msg := strings.TrimSpace(m.ta.Value())
	m.ta.Reset()
	m.ta.Focus()
	if msg == "" {
		return m, nil
	}
	// 待确认的危险操作：输入 y/n 决策
	if t.approvalCh != nil {
		t.handleApproval(msg)
		m.refreshChat()
		return m, nil
	}
	// 首次配置引导：拦截输入走引导状态机
	if t.setupState != setupNone {
		t.handleSetup(msg)
		m.refreshChat()
		return m, nil
	}
	if strings.HasPrefix(msg, "/") {
		if err := t.handleCommand(msg); err != nil {
			if errors.Is(err, errQuit) {
				return m, tea.Quit
			}
			t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		}
		m.refreshChat()
		return m, nil
	}
	if t.status.busy {
		t.chat.appendSystem("(still processing the previous message, please wait)")
		m.refreshChat()
		return m, nil
	}
	// 同步置位 busy 并显示用户消息（消息循环线程内完成，无 TOCTOU 窗口）；
	// runAgent 在后台 goroutine 运行，完成经 agentDoneMsg 回投。
	t.status.setBusy(true)
	t.chat.appendUser(msg)
	t.chat.startProcessing() // 同步立即显示 [processing...]（生成结束由 runAgent 置 done）
	m.refreshChat()
	go t.runAgent(msg)
	return m, nil
}

// handleAgentEvent 按事件类型更新聊天区与状态栏（逻辑与 gocui 版 consumeEvents 一致）。
func (m model) handleAgentEvent(ev agent.Event) {
	t := m.t
	switch e := ev.(type) {
	case agent.TextDeltaEvent:
		t.chat.appendAssistantDelta(e.Text)
	case agent.TextDoneEvent:
		// 已通过增量渲染，无需额外处理
	case agent.ToolCallEvent:
		t.chat.appendToolCall(e)
	case agent.ToolResultEvent:
		t.chat.finishToolCall(e)
	case agent.ModeChangeEvent:
		t.status.setMode(e.Mode)
	case agent.ThinkingStartEvent:
		// 后端返回思维链：processing → thinking
		t.chat.confirmThinking()
	case agent.DoneEvent:
		t.status.setUsage(e.Usage)
	}
}

// View 渲染整屏：聊天区 + 状态栏（边框盒）+ 输入区。
// 布局行数：viewport(H-7) + 1 空行 + 边框 3 + 输入区 3 = H 行（M0 实测约束）。
func (m model) View() string {
	if m.width == 0 || m.height == 0 || m.vp == nil {
		return "loading..."
	}
	var b strings.Builder
	chatStr := m.vp.View()
	if m.t.pickerOpen() {
		chatStr = m.overlayChat(chatStr)
	}
	b.WriteString(chatStr)
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().
		Width(m.width).
		Border(lipgloss.RoundedBorder(), true, true, true, true).
		BorderForeground(lipgloss.Color("240")).
		Render(m.t.status.title()))
	b.WriteString("\n")
	b.WriteString(m.ta.View())
	return b.String()
}

// overlayChat 把弹窗盒子插入聊天区中央。
// 按行切分拼接（不触碰行内 ANSI 序列），总行数保持不变（H-7），
// 避免超出屏幕高度被终端滚动截断（M0 实测约束）。
func (m model) overlayChat(chat string) string {
	box := m.t.renderPickerBox(m.width, m.height)
	if box == "" {
		return chat
	}
	chat = strings.TrimSuffix(chat, "\n")
	chatLines := strings.Split(chat, "\n")
	boxLines := strings.Split(box, "\n")

	boxH := len(boxLines)
	if boxH > len(chatLines) {
		boxH = len(chatLines)
	}
	y0 := (len(chatLines) - boxH) / 2
	if y0 < 0 {
		y0 = 0
	}

	var b strings.Builder
	b.WriteString(strings.Join(chatLines[:y0], "\n"))
	b.WriteString("\n")
	b.WriteString(strings.Join(boxLines[:boxH], "\n"))
	if y0+boxH < len(chatLines) {
		b.WriteString("\n")
		b.WriteString(strings.Join(chatLines[y0+boxH:], "\n"))
	}
	return b.String()
}
