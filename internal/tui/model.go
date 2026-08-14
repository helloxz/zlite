package tui

import (
	"errors"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
	t *TUI
	// vp 用指针字段：viewport.Model 是值类型，若内嵌值拷贝，值接收者方法内
	// 的 SetContent/PageUp 等 *Model 方法只会修改临时拷贝（M1 实测 bug）。
	vp     *viewport.Model
	ta     textarea.Model
	width  int
	height int
}

func newModel(t *TUI) model {
	ta := textarea.New()
	ta.Placeholder = "Type a message... (Enter=send, Tab=mode, Ctrl+C=quit)"
	ta.ShowLineNumbers = false
	// 每行蓝色竖线前缀（与圆角边框风格统一；Prompt 必须早于 SetWidth 设置，
	// handleResize 中调用——顺序天然满足）。
	ta.Prompt = "│ "
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	ta.BlurredStyle.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	// 输入区全宽深灰背景（与头带 236 一致）+ 内容左右 padding 各 1 列；
	// textarea 内部按 Base frame size 计算内容宽，SetWidth 传屏幕全宽。
	ta.FocusedStyle.Base = lipgloss.NewStyle().Background(lipgloss.Color("236")).Padding(0, 1)
	ta.BlurredStyle.Base = lipgloss.NewStyle().Background(lipgloss.Color("236")).Padding(0, 1)
	// 默认 CursorLine 背景是黑色（AdaptiveColor Dark "0"），会覆盖 Base 的
	// 236 背景导致光标行色块突兀；显式统一为深灰。
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle().Background(lipgloss.Color("236"))
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
// + 输入区（2 行）。行数必须精确等于屏幕高，否则底部留白：
// viewport(H-5) + 状态栏 3 + 输入区 2 = H。
func (m model) handleResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width, m.height = msg.Width, msg.Height
	m.t.screenH = msg.Height // 弹窗布局用
	m.t.screenW = msg.Width
	v := viewport.New(msg.Width, msg.Height-5)
	m.vp = &v
	m.ta.SetWidth(msg.Width) // 全宽背景；内容宽由 textarea 按 padding/prompt 自动折算
	m.ta.SetHeight(2)        // 2 行可视：内容换行可见（inputAreaView 做行交换）
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
		cmd, _ := t.handleSetup(msg)
		m.refreshChat()
		return m, cmd
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
	} else if hints := m.commandHint(); len(hints) > 0 {
		// 输入 / 开头的命令提示浮层（非模态，覆盖聊天区底部多行）
		chatStr = m.overlayHint(chatStr, hints)
	}
	b.WriteString(chatStr)
	b.WriteString("\n")
	// 状态栏：Width(m.width-2) 保证总宽 = 屏幕宽（Width 不含 border，
	// 此前 Width(m.width) 会使总宽超 2 列、右框被终端截断——顺带修复）。
	b.WriteString(lipgloss.NewStyle().
		Width(m.width - 2).
		Border(lipgloss.RoundedBorder(), true, true, true, true).
		BorderForeground(lipgloss.Color("240")).
		Render(m.t.status.title(m.width - 2)))
	b.WriteString("\n")
	b.WriteString(m.inputAreaView())
	return b.String()
}

// inputAreaView 渲染输入区（textarea 2 行）。
//
// 行交换说明：bubbletea alt screen 渲染器每帧把终端硬件光标强制定位到
// View 最后一行行首（standard_renderer.flush，保证增量渲染一致）；IME
// preedit 由终端绘制在硬件光标处。因此内容不足 2 行时把「内容行」交换到
// 最后一行（空行在上），让硬件光标落在内容行——拼音显示在内容行行首
// （内容前方；行内跟随受框架限制，见 docs §0.11）。
func (m model) inputAreaView() string {
	v := strings.TrimSuffix(m.ta.View(), "\n")
	lines := strings.Split(v, "\n")
	// 内容不足 2 行（单行内容/placeholder）时：空行在上、内容行在下。
	// 内容行必须在 View 输出末尾——终端硬件光标（=IME 拼音位置）由渲染器
	// 停在最后一行，与 textarea 虚拟光标一致（M0/M1 已验证）。
	// 注意：不能用 TrimSpace 判空——ta 输出行带 prompt "│ "，永远非空。
	if len(lines) == 2 && isEmptyPromptLine(lines[1]) {
		lines[0], lines[1] = lines[1], lines[0]
	}
	return strings.Join(lines, "\n")
}

// isEmptyPromptLine 判断 textarea 输出行是否仅含 padding/prompt/空白（无内容）。
func isEmptyPromptLine(s string) bool {
	s = strings.TrimSpace(s)        // 去首尾 padding/pad
	s = strings.TrimPrefix(s, "│ ") // 去 prompt（含尾随空格）
	s = strings.TrimPrefix(s, "│")  // 容错：无尾随空格的 prompt
	return strings.TrimSpace(s) == ""
}

// commandHint 返回输入框以 / 开头时的命令提示（每行一个命令 + 描述，
// 按前缀过滤；非 / 开头、无匹配或弹窗打开时返回 nil）。多行输入只取
// 首行参与匹配。
func (m model) commandHint() []string {
	val := m.ta.Value()
	if i := strings.IndexByte(val, '\n'); i >= 0 {
		val = val[:i]
	}
	if !strings.HasPrefix(val, "/") {
		return nil
	}
	var out []string
	nameW := hintNameWidth()
	for _, c := range commandInfos {
		if strings.HasPrefix(c.name, val) {
			out = append(out, padDisplay(c.name, nameW)+c.desc)
		}
	}
	return out
}

// overlayHint 把命令提示逐行覆盖到聊天区底部（每行一个命令 + 描述，
// 灰色；不改变布局行数，聊天区内容只是被遮挡，滚动后仍可见）。
// 覆盖行数取命令数与聊天区行数的较小值（从底部向上）。
// 行首无 ANSI 时整行替换安全（被替换行整体丢弃，不破坏行内序列）。
func (m model) overlayHint(chat string, hints []string) string {
	chat = strings.TrimSuffix(chat, "\n")
	lines := strings.Split(chat, "\n")
	if len(lines) == 0 {
		return chat
	}
	n := len(hints)
	if n > len(lines) {
		n = len(lines)
	}
	start := len(lines) - n
	for i := 0; i < n; i++ {
		line := ansiPathGray + truncateDisplay(hints[i], m.width-1) + ansiReset
		if dw := displayWidth(line); dw < m.width {
			line += strings.Repeat(" ", m.width-dw)
		}
		lines[start+i] = line
	}
	return strings.Join(lines, "\n")
}
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
	// 水平居中：盒子行加 x0 个前缀空格（行首无 ANSI，安全）；
	// 盒宽取首行显示宽度（ansi.StringWidth 忽略 SGR）。
	boxW := ansi.StringWidth(boxLines[0])
	x0 := (m.width - boxW) / 2
	if x0 < 0 {
		x0 = 0
	}
	pad := strings.Repeat(" ", x0)
	for i, bl := range boxLines[:boxH] {
		b.WriteString(pad + bl)
		if i < boxH-1 {
			b.WriteString("\n")
		}
	}
	if y0+boxH < len(chatLines) {
		b.WriteString("\n")
		b.WriteString(strings.Join(chatLines[y0+boxH:], "\n"))
	}
	return b.String()
}
