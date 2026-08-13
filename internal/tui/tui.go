// Package tui 实现 zlite 的终端界面（gocui）。
//
// TUI 只消费 agent 的事件流并触发 agent.Run，不包含任何业务逻辑；
// 二期 ACP 接入时通过同一事件契约复用 agent 核心。
package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/awesome-gocui/gocui"
	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/config"
)

// agentFace 是 TUI 依赖的 agent 能力（测试可注入 fake）。
type agentFace interface {
	Events() <-chan agent.Event
	Run(ctx context.Context, msg string) error
	RunInit(ctx context.Context, msg string) error
	SetMode(m agent.Mode)
	Mode() agent.Mode
}

const (
	chatViewName  = "chat"
	inputViewName = "input"
)

// TUI 是终端界面。
type TUI struct {
	cfg   *config.Config
	agent agentFace // 引导完成前为 nil（首次配置流程）
	model string
	cwd   string

	// newSession 创建新会话并切换到它（/new 命令用，由 app.go 注入；
	// TUI 不持有 session 管理逻辑，保持零业务）。
	newSession func() error

	// models 是可选模型列表（/switch 用，来自配置 providers[0].models）；
	// switchModel 执行实际切换（app.go 注入：重建模型流并注入 agent）。
	models      []string
	switchModel func(name string) error
	// pickerSel 是模型选择弹窗的当前选中行（仅主循环线程读写）。
	pickerSel int

	// setupNeeded 为 true 时进入首次配置引导（type → base_url → api_key → models），
	// 完成后调用 onSetupDone 落盘并热重载（app.go 注入）。
	setupNeeded bool
	onSetupDone func(config.SetupInput) error
	setupState  setupState
	setupInput  config.SetupInput

	g      *gocui.Gui
	chat   *chatView
	status *statusView

	// uiTasks 串行化所有 UI 更新：g.Update 内部会新开 goroutine 投递，
	// 并发调用时执行顺序不保证（曾出现流式增量颠倒）；
	// 统一经 FIFO 队列 + 单分发 goroutine 串行 UpdateAsync。
	uiTasks chan func()

	// approvalCh 非 nil 时表示有正在等待用户确认的危险操作
	// （仅主循环线程读写：Approver 经 ui 队列设置，submit 消费）。
	approvalCh chan agent.ApprovalDecision

	ctx    context.Context
	cancel context.CancelFunc
}

// New 创建 TUI。newSession 为 /new 命令的会话切换回调（可为 nil）。
func New(cfg *config.Config, a agentFace, model, cwd string, newSession func() error) *TUI {
	ctx, cancel := context.WithCancel(context.Background())
	return &TUI{
		cfg: cfg, agent: a, model: model, cwd: cwd,
		newSession: newSession,
		uiTasks:    make(chan func(), 128),
		ctx:        ctx, cancel: cancel,
	}
}

// setupState 是首次配置引导的步骤状态机。
type setupState int

const (
	setupNone setupState = iota
	setupType
	setupBaseURL
	setupAPIKey
	setupModels
)

// EnableSetup 启用首次配置引导（必须在 Run 前调用）。
// onSetupDone 在引导完成后回调（落盘 + 热重载），返回错误则引导重新开始。
func (t *TUI) EnableSetup(onSetupDone func(config.SetupInput) error) {
	t.setupNeeded = true
	t.onSetupDone = onSetupDone
}

// SetAgent 替换 agent（引导完成热重载后调用）。
func (t *TUI) SetAgent(a agentFace) {
	t.agent = a
}

// SetModel 更新状态栏模型名（引导完成热重载后调用）。
func (t *TUI) SetModel(name string) {
	t.model = name
	if t.status != nil {
		t.status.setModel(name)
	}
}

// SetNewSession 替换 /new 回调（引导完成热重载后调用）。
func (t *TUI) SetNewSession(fn func() error) {
	t.newSession = fn
}

// SetSwitchModel 配置 /switch 命令：models 为可选模型列表（来自配置
// providers[0].models），fn 执行实际切换（由 app.go 注入）。
func (t *TUI) SetSwitchModel(models []string, fn func(name string) error) {
	t.models = models
	t.switchModel = fn
}

// ui 把 UI 更新任务加入串行队列（FIFO，顺序保证）。
func (t *TUI) ui(f func()) {
	t.uiTasks <- f
}

// dispatch 从队列取出任务并投递到 gocui 主循环（单 goroutine，串行）。
func (t *TUI) dispatch() {
	for f := range t.uiTasks {
		t.g.UpdateAsync(func(*gocui.Gui) error { f(); return nil })
	}
}

// Run 启动 TUI（阻塞直到退出）。
func (t *TUI) Run() error {
	g, err := gocui.NewGui(gocui.OutputNormal, true)
	if err != nil {
		return fmt.Errorf("初始化终端界面失败: %w", err)
	}
	return t.run(g)
}

// run 在指定 Gui 上运行主循环（测试可传入 OutputSimulator 的 Gui）。
func (t *TUI) run(g *gocui.Gui) error {
	t.g = g
	defer g.Close()

	t.setup(g)
	go t.dispatch()
	if t.agent != nil {
		go t.consumeEvents()
	}
	// 首次配置引导：等 MainLoop 首个循环创建好视图后启动（UpdateAsync 投递）
	if t.setupNeeded {
		g.UpdateAsync(func(*gocui.Gui) error { t.startSetup(); return nil })
	}

	if err := g.MainLoop(); err != nil && !errors.Is(err, gocui.ErrQuit) {
		return err
	}
	t.cancel() // 终止未完成的 agent 运行
	return nil
}

// setup 布局与键位绑定。
func (t *TUI) setup(g *gocui.Gui) {
	// 边框用 ASCII 字符（- | +）：框线字符（│ ─ ┌ 等）的 East Asian Width
	// 为 Ambiguous，在 CJK locale 下 tcell 按宽 2 渲染，会覆盖内容第一列
	// （实测现象：每行开头吞掉 1 个字符）。ASCII 边框宽度固定 1，不受 locale 影响。
	g.ASCII = true
	// 显示并定位光标到输入框：gocui 默认隐藏光标，中文输入法（IME）的 preedit
	// 预览会显示在终端默认光标位置（屏幕右下）并触发重绘闪烁。
	g.Cursor = true

	g.SetManagerFunc(t.layout)

	// 全局：Ctrl+C 退出
	g.SetKeybinding("", gocui.KeyCtrlC, gocui.ModNone, t.quit)
	// 输入区：Enter 提交
	g.SetKeybinding(inputViewName, gocui.KeyEnter, gocui.ModNone, t.submit)
	// Tab 切换 plan/build 模式（view 级优先于 DefaultEditor 的 tab 插入）
	g.SetKeybinding(inputViewName, gocui.KeyTab, gocui.ModNone, t.toggleMode)
	g.SetKeybinding("", gocui.KeyTab, gocui.ModNone, t.toggleMode)
}

// layout 两区布局：消息区（带边框）+ 输入区（3 行高带边框，内容区 1 行；
// 状态信息（模式/模型/用量）显示在输入框边框的 Title 上）。
// 说明：gocui 的 setRune 固定 +1 边框偏移，1 行无边框视图的内容会画到屏幕外，
// 因此输入框必须带边框且高度 ≥3 才有内容区。
func (t *TUI) layout(g *gocui.Gui) error {
	maxX, maxY := g.Size()
	if maxX <= 0 || maxY <= 0 {
		return nil
	}

	// 输入区（底部 3 行，边框，内容区 1 行）
	if v, err := g.SetView(inputViewName, 0, maxY-3, maxX-1, maxY-1, 0); err != nil {
		if !errors.Is(err, gocui.ErrUnknownView) {
			return err
		}
		v.Editable = true
		v.Editor = gocui.DefaultEditor
		// Wrap=true：光标位置换算走累加字符宽度的分支（linesPosOnScreen 在
		// Wrap=false 时把字符索引当列坐标，中文宽 2 导致光标落后半个字）。
		v.Wrap = true
		t.status = newStatusView(v, t.model)
		t.status.render()
		if _, err := g.SetCurrentView(inputViewName); err != nil {
			return err
		}
	}

	// 消息区（占主体，带边框）
	if v, err := g.SetView(chatViewName, 0, 0, maxX-1, maxY-4, 0); err != nil {
		if !errors.Is(err, gocui.ErrUnknownView) {
			return err
		}
		t.chat = newChatView(v)
		t.chat.appendSystem("zlite ready - " + t.cwd)
	}

	// 模型选择弹窗 view 常驻（启动时创建一次，Visible=false 隐藏）：
	// 运行期只切 Visible/坐标，不增删 g.views——gocui 的 loaderTick 会
	// 并发遍历 views，增删存在数据竞争（见 model_picker.go 注释）。
	if _, err := g.View(modelPickerViewName); err != nil {
		v, err := g.SetView(modelPickerViewName, 0, 0, 1, 1, 0)
		if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
			return err
		}
		v.Visible = false
		v.Title = " Select model "
		v.Highlight = true // 光标所在行用 Sel 配色高亮
		v.SelBgColor = gocui.ColorCyan
		v.SelFgColor = gocui.ColorBlack
	}

	return nil
}

// submit 提交输入：先处理待确认操作，再处理斜杠命令，否则交给 agent。
func (t *TUI) submit(g *gocui.Gui, v *gocui.View) error {
	msg := strings.TrimSpace(v.Buffer())
	v.Clear()
	v.SetCursor(0, 0)
	v.SetOrigin(0, 0)
	if msg == "" {
		return nil
	}
	// 待确认的危险操作：输入 y/n 决策
	if t.approvalCh != nil {
		return t.handleApproval(msg)
	}
	// 首次配置引导：拦截输入走引导状态机
	if t.setupState != setupNone {
		return t.handleSetup(msg)
	}
	if strings.HasPrefix(msg, "/") {
		return t.handleCommand(msg)
	}
	if t.status.busy {
		t.chat.appendSystem("(still processing the previous message, please wait)")
		return nil
	}
	// 同步置位 busy 并显示用户消息（主循环线程）：runAgent 在后台
	// goroutine 运行，若经异步 ui 队列置位，后续 /switch 的 busy 检查
	// 存在 TOCTOU 窗口。
	t.status.setBusy(true)
	t.chat.appendUser(msg)
	go t.runAgent(msg)
	return nil
}

// handleApproval 处理确认输入（y 批准 / n 拒绝）。
func (t *TUI) handleApproval(msg string) error {
	ch := t.approvalCh
	switch msg {
	case "y", "Y", "yes", "Yes":
		t.approvalCh = nil
		ch <- agent.Approved
		t.chat.appendSystem(colorize("Approved", ansiGreen))
	case "n", "N", "no", "No":
		t.approvalCh = nil
		ch <- agent.Denied
		t.chat.appendSystem(colorize("Denied", ansiRed))
	default:
		t.chat.appendSystem("Please answer y (approve) or n (deny)")
	}
	return nil
}

// runAgent 在后台执行一轮对话（busy 与用户消息已由 submit 同步处理）。
func (t *TUI) runAgent(msg string) {
	err := t.agent.Run(t.ctx, msg)
	t.ui(func() {
		t.status.setBusy(false)
		if err != nil {
			t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		}
	})
}

// handleCommand 处理斜杠命令（支持参数："/init <要求>"）。
func (t *TUI) handleCommand(cmd string) error {
	name, args := splitCommand(cmd)
	switch name {
	case "/exit", "/quit":
		return gocui.ErrQuit
	case "/new":
		return t.newChat()
	case "/plan":
		t.switchMode(agent.ModePlan)
	case "/build":
		t.switchMode(agent.ModeBuild)
	case "/switch":
		return t.handleSwitch(args)
	case "/init":
		return t.initProject(args)
	case "/help":
		t.chat.appendSystem(helpText)
	default:
		t.chat.appendSystem("Unknown command: " + cmd + " (type /help for available commands)")
	}
	return nil
}

// handleSwitch 处理 /switch：带参数直接切换；无参数弹出模型选择列表。
// 与 /new 一致：agent 忙碌时拒绝，避免替换 streamer 与生成中的 runOnce 竞争。
func (t *TUI) handleSwitch(args string) error {
	if t.status.busy {
		t.chat.appendSystem("(still processing the previous message, please wait)")
		return nil
	}
	if t.switchModel == nil {
		t.chat.appendSystem(colorize("Error: /switch is unavailable", ansiRed))
		return nil
	}
	if args == "" {
		return t.openModelPicker()
	}
	return t.switchToModel(args)
}

// switchToModel 校验模型名并执行切换（弹窗确认与 /switch <name> 共用）。
func (t *TUI) switchToModel(name string) error {
	if !slices.Contains(t.models, name) {
		t.chat.appendSystem(colorize("Unknown model: "+name+" (available: "+strings.Join(t.models, ", ")+")", ansiRed))
		return nil
	}
	if err := t.switchModel(name); err != nil {
		t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		return nil
	}
	t.SetModel(name)
	t.chat.appendSystem("Switched to model: " + name)
	return nil
}

// splitCommand 把斜杠命令拆分为命令名与参数："/init 补充要求" → ("/init", "补充要求")。
func splitCommand(cmd string) (name, args string) {
	trimmed := strings.TrimSpace(cmd)
	if i := strings.IndexByte(trimmed, ' '); i >= 0 {
		return trimmed[:i], strings.TrimSpace(trimmed[i+1:])
	}
	return trimmed, ""
}

// initProject 执行 /init：plan 模式提示（只输出内容），build 模式写入文件。
// args 是用户在命令后附加的补充要求（随用户消息传给模型）。
func (t *TUI) initProject(args string) error {
	if t.status.busy {
		t.chat.appendSystem("(still processing the previous message, please wait)")
		return nil
	}
	if t.agent.Mode() == agent.ModePlan {
		t.chat.appendSystem("Running /init in plan mode: AGENTS.md content will be shown here. Switch to build mode (Tab or /build) and run /init again to write it to disk.")
	} else {
		t.chat.appendSystem("Running /init: scanning the project and writing AGENTS.md...")
	}
	msg := "/init"
	if args != "" {
		msg = "/init " + args // 附加要求作为用户消息传给模型（会话记录完整）
	}
	t.status.setBusy(true) // 同步置位（同 submit：消除 busy 检查的 TOCTOU 窗口）
	go t.runInit(msg)
	return nil
}

// runInit 在后台执行 init 任务（busy 已由 initProject 同步置位）。
func (t *TUI) runInit(msg string) {
	err := t.agent.RunInit(t.ctx, msg)
	t.ui(func() {
		t.status.setBusy(false)
		if err != nil {
			t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		}
	})
}

// newChat 新建会话（/new）：结束当前会话、创建新会话、重置模式为 plan。
func (t *TUI) newChat() error {
	if t.status.busy {
		t.chat.appendSystem("(still processing the previous message, please wait)")
		return nil
	}
	if t.newSession == nil {
		t.chat.appendSystem(colorize("Error: /new is unavailable", ansiRed))
		return nil
	}
	if err := t.newSession(); err != nil {
		t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		return nil
	}
	t.chat.reset()
	t.chat.appendSystem("New session started")
	t.agent.SetMode(agent.ModePlan) // /new 后模式重置为 plan
	return nil
}

// switchMode 切换模式并提示（Tab 与 /plan /build 共用）。
func (t *TUI) switchMode(m agent.Mode) {
	if t.agent == nil { // 引导完成前无 agent，忽略 Tab
		return
	}
	if t.agent.Mode() == m {
		return
	}
	t.agent.SetMode(m)
	if m == agent.ModePlan {
		t.chat.appendSystem("Switched to plan mode (read-only)")
	} else {
		t.chat.appendSystem("Switched to build mode (writable)")
	}
}

// toggleMode 在 plan 与 build 之间切换（Tab 键）。
func (t *TUI) toggleMode(g *gocui.Gui, v *gocui.View) error {
	if t.agent.Mode() == agent.ModePlan {
		t.switchMode(agent.ModeBuild)
	} else {
		t.switchMode(agent.ModePlan)
	}
	return nil
}

// consumeEvents 消费 agent 事件流并更新界面（goroutine）。
func (t *TUI) consumeEvents() {
	for ev := range t.agent.Events() {
		switch e := ev.(type) {
		case agent.TextDeltaEvent:
			t.ui(func() { t.chat.appendAssistantDelta(e.Text) })
		case agent.TextDoneEvent:
			// 已通过增量渲染，无需额外处理
		case agent.ToolCallEvent:
			t.ui(func() { t.chat.appendToolCall(e) })
		case agent.ToolResultEvent:
			t.ui(func() { t.chat.finishToolCall(e) })
		case agent.ModeChangeEvent:
			t.ui(func() { t.status.setMode(e.Mode) })
		case agent.DoneEvent:
			t.ui(func() { t.status.setUsage(e.Usage) })
		}
	}
}

// quit 退出 TUI。
func (t *TUI) quit(g *gocui.Gui, v *gocui.View) error {
	return gocui.ErrQuit
}

// Stop 请求 TUI 退出（外部信号处理用）。
// 回调返回 ErrQuit 使 MainLoop 正常退出；TUI 未启动（g 为 nil）时忽略。
func (t *TUI) Stop() {
	if t.g == nil {
		return
	}
	t.g.Update(func(*gocui.Gui) error { return gocui.ErrQuit })
}

const helpText = `Available commands:
  /init    Analyze the project and generate/refresh AGENTS.md
  /new     Start a new session (mode resets to plan)
  /plan    Switch to plan mode (read-only: inspect and search only)
  /build   Switch to build mode (writable: modify files, run commands)
  /switch  Switch model (pick from the list, or /switch <model>)
  /exit    Quit zlite
  /help    Show this help

Usage: type a message and press Enter to send; Tab toggles plan/build mode; Ctrl+C to quit.`

// ---- 首次配置引导（setup state machine）----

// startSetup 启动引导：输出欢迎语与第一步提问。
// 仅在 MainLoop 首个循环后调用（chat view 已就绪）。
func (t *TUI) startSetup() {
	t.setupState = setupType
	t.chat.appendSystem("Welcome to zlite! Let's configure your model provider.")
	t.chat.appendSystem("Type: enter 1 for openai.chat, 2 for openai.responses:")
}

// handleSetup 按当前步骤处理用户输入（submit 拦截）。
// api_key 必填（用户决策），其余非法输入提示重输；全部完成调用
// onSetupDone 落盘并热重载，失败则引导从头再来。
func (t *TUI) handleSetup(msg string) error {
	switch t.setupState {
	case setupType:
		switch msg {
		case "1":
			t.setupInput.Type = config.TypeOpenAIChat
		case "2":
			t.setupInput.Type = config.TypeOpenAIResponses
		default:
			t.chat.appendSystem("Invalid choice. Enter 1 (openai.chat) or 2 (openai.responses):")
			return nil
		}
		t.setupState = setupBaseURL
		t.chat.appendSystem("Base URL (e.g. https://api.example.com/v1):")
	case setupBaseURL:
		if msg == "" {
			t.chat.appendSystem("Base URL cannot be empty. Enter the endpoint base URL:")
			return nil
		}
		t.setupInput.BaseURL = msg
		t.setupState = setupAPIKey
		t.chat.appendSystem("API key (saved to ~/.zlite/.env as ZLITE_API_KEY):")
	case setupAPIKey:
		if msg == "" {
			t.chat.appendSystem("API key cannot be empty. Enter your API key:")
			return nil
		}
		t.setupInput.APIKey = msg
		t.setupState = setupModels
		t.chat.appendSystem("Model(s), comma separated (e.g. gpt-4o, gpt-4o-mini):")
	case setupModels:
		models := config.SplitModels(msg)
		if len(models) == 0 {
			t.chat.appendSystem("At least one model is required. Enter model(s), comma separated:")
			return nil
		}
		t.setupInput.Models = models
		t.setupState = setupNone
		t.status.setBusy(true)
		err := t.onSetupDone(t.setupInput)
		t.status.setBusy(false)
		if err != nil {
			t.chat.appendSystem(colorize("Setup failed: "+err.Error(), ansiRed))
			t.chat.appendSystem("Let's try again. Type: enter 1 for openai.chat, 2 for openai.responses:")
			t.setupState = setupType
			t.setupInput = config.SetupInput{}
			return nil
		}
		t.chat.appendSystem(colorize("Configuration saved. Happy coding!", ansiGreen))
	}
	return nil
}
