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
	"github.com/helloxz/zlite/internal/llm"
)

// agentFace 是 TUI 依赖的 agent 能力（测试可注入 fake）。
type agentFace interface {
	Events() <-chan agent.Event
	Run(ctx context.Context, msg string) error
	RunInit(ctx context.Context, msg string) error
	SetMode(m agent.Mode)
	Mode() agent.Mode
	SetThinking(t string)
	History() []llm.Message // 当前会话历史（/sessions 切换后渲染用）
}

const (
	chatViewName  = "chat"
	inputViewName = "input"
)

// thinkingEfforts 是可选思考强度列表（/thinking 与 Ctrl+T 用）。
// auto 表示不传 reasoning_effort 参数，由 API 自行决定；其余值原样透传，
// 是否支持由后端决定（不支持时 API 报错，错误透传给用户展示）。
var thinkingEfforts = []string{"none", "auto", "low", "medium", "high", "xhigh", "max"}

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

	// sessionItems 返回可切换的会话列表（/sessions 用，app.go 注入：
	// 最近 20 条，含标题与创建时间）；switchSession 执行会话切换。
	sessionItems  func() ([]SessionItem, error)
	switchSession func(id string) error

	// skillsLister 返回已发现的 skills 列表（/skills 用，app.go 注入）。
	skillsLister func() []SkillItem

	// thinking 是当前思考强度显示值（默认 auto；与 agent 侧实际传参
	// 对应：auto 归一化为不传）。
	thinking string

	// 列表弹窗状态（/switch 与 /sessions 共用，仅主循环线程读写）。
	pickerLabels []string
	pickerValues []string
	pickerOnPick func(g *gocui.Gui, value string) error
	pickerSel    int

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
		thinking:   "auto",
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

// SessionItem 是 /sessions 列表中的一项（app.go 把 session.Info 转换后注入）。
type SessionItem struct {
	ID    string // 会话 ID（切换时传给 switchSession）
	Title string // 会话标题（可为空，UI 显示 (no title)）
	Time  string // 创建时间（已格式化，如 "01-02 15:04"）
}

// SetSwitchModel 配置 /switch 命令：models 为可选模型列表（来自配置
// providers[0].models），fn 执行实际切换（由 app.go 注入）。
func (t *TUI) SetSwitchModel(models []string, fn func(name string) error) {
	t.models = models
	t.switchModel = fn
}

// SetSessionSwitcher 配置 /sessions 命令（Ctrl+L 共用）：
// items 返回可切换的会话列表（最近 20 条），fn 按会话 ID 执行切换。
func (t *TUI) SetSessionSwitcher(items func() ([]SessionItem, error), fn func(id string) error) {
	t.sessionItems = items
	t.switchSession = fn
}

// SkillItem 是 /skills 列表中的一项（app.go 把 skills.SkillInfo 转换后注入）。
type SkillItem struct {
	Name        string // frontmatter name
	Description string // frontmatter description
	Source      string // "global" | "project"（来源层级）
	Path        string // SKILL.md 绝对路径
}

// SetSkillsLister 配置 /skills 命令：返回已发现的 skills 列表（可为空）。
func (t *TUI) SetSkillsLister(fn func() []SkillItem) {
	t.skillsLister = fn
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
	// Output256：头带用 236/237 深灰；8 色 SGR 仍走 outputNormal 回退。
	g, err := gocui.NewGui(gocui.Output256, true)
	if err != nil {
		return fmt.Errorf("初始化终端界面失败: %w", err)
	}
	return t.run(g)
}

// run 在指定 Gui 上运行主循环（测试可传入 OutputSimulator 的 Gui）。
func (t *TUI) run(g *gocui.Gui) error {
	t.g = g
	// 刻意不启用 g.Mouse（保持 false）：请求鼠标事件（mouse tracking）会
	// 让终端模拟器放弃原生文本选择，拖拽选字/复制失效。滚动一律走键盘
	// （PgUp/PgDn 翻页、Home/End 到顶/到底），与 opencode 等工具同策略。
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
	// Ctrl+L 打开会话列表（/sessions 等效；绑在 input view，弹窗打开时不触发）
	g.SetKeybinding(inputViewName, gocui.KeyCtrlL, gocui.ModNone, t.openSessionsKey)
	// Ctrl+N 新建会话（/new 等效）
	g.SetKeybinding(inputViewName, gocui.KeyCtrlN, gocui.ModNone, t.newChatKey)
	// Ctrl+T 弹出思考强度选择（/thinking 等效）
	g.SetKeybinding(inputViewName, gocui.KeyCtrlT, gocui.ModNone, t.thinkingKey)
	// Shift+Tab 弹出模型切换（/switch 等效）；ModNone/ModShift 双绑定，
	// 兜底部分终端对 Shift+Tab 带显式 Shift 修饰（CSI 1;2Z）的情况
	g.SetKeybinding(inputViewName, gocui.KeyBacktab, gocui.ModNone, t.switchModelKey)
	g.SetKeybinding(inputViewName, gocui.KeyBacktab, gocui.ModShift, t.switchModelKey)
	// Tab 切换 plan/build 模式（view 级优先于 DefaultEditor 的 tab 插入）
	g.SetKeybinding(inputViewName, gocui.KeyTab, gocui.ModNone, t.toggleMode)
	g.SetKeybinding("", gocui.KeyTab, gocui.ModNone, t.toggleMode)
	// 聊天区滚动：PgUp/PgDn 翻页、Home/End 直接到顶/到底。
	// 不请求鼠标事件（见 run 注释），滚动纯键盘。
	// 全局绑定安全：输入框的 DefaultEditor 不使用这些键（无编辑冲突）；
	// 弹窗打开时 handler 内忽略（见 chatScroll）。
	g.SetKeybinding("", gocui.KeyPgup, gocui.ModNone, t.chatScroll(-1))
	g.SetKeybinding("", gocui.KeyPgdn, gocui.ModNone, t.chatScroll(1))
	g.SetKeybinding("", gocui.KeyHome, gocui.ModNone, t.chatJumpTop)
	g.SetKeybinding("", gocui.KeyEnd, gocui.ModNone, t.chatJumpBottom)
}

// chatScroll 生成聊天区翻页 handler：dy 为方向（-1 上翻 / +1 下翻），
// 一页 = 可视高度 - 1 行（避免滚动过头）。列表弹窗打开时忽略滚动。
func (t *TUI) chatScroll(dy int) func(g *gocui.Gui, v *gocui.View) error {
	return func(g *gocui.Gui, v *gocui.View) error {
		if t.chat == nil {
			return nil
		}
		if cv := g.CurrentView(); cv != nil && cv.Name() == pickerViewName {
			return nil // 弹窗内 PgUp/PgDn 不滚动聊天区
		}
		_, maxY := t.chat.view.Size()
		t.chat.scrollBy(dy * (maxY - 1))
		return nil
	}
}

// chatJumpTop 直接滚动到聊天区顶部（Home）。列表弹窗打开时忽略。
func (t *TUI) chatJumpTop(g *gocui.Gui, v *gocui.View) error {
	if t.chat == nil {
		return nil
	}
	if cv := g.CurrentView(); cv != nil && cv.Name() == pickerViewName {
		return nil
	}
	t.chat.scrollToTop()
	return nil
}

// chatJumpBottom 直接滚动到聊天区底部并恢复自动跟随（End）。
// 列表弹窗打开时忽略。
func (t *TUI) chatJumpBottom(g *gocui.Gui, v *gocui.View) error {
	if t.chat == nil {
		return nil
	}
	if cv := g.CurrentView(); cv != nil && cv.Name() == pickerViewName {
		return nil
	}
	t.chat.scrollToBottom()
	return nil
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
	} else if t.chat != nil {
		t.chat.relayout()
	}

	// 列表选择弹窗 view 常驻（启动时创建一次，Visible=false 隐藏）：
	// 运行期只切 Visible/坐标，不增删 g.views——gocui 的 loaderTick 会
	// 并发遍历 views，增删存在数据竞争（见 picker.go 注释）。
	if _, err := g.View(pickerViewName); err != nil {
		v, err := g.SetView(pickerViewName, 0, 0, 1, 1, 0)
		if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
			return err
		}
		v.Visible = false
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
	t.chat.startProcessing() // 同步立即显示 [processing...]（生成结束由 runAgent 置 done）
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
		t.chat.finishProcessing() // 整轮生成结束：统一置 [done]
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
	case "/thinking":
		return t.handleThinking(args)
	case "/sessions":
		return t.handleSessions()
	case "/skills":
		return t.handleSkills()
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

// openModelPicker 弹出模型选择列表（/switch 无参数时调用），
// 默认定位到当前模型。
func (t *TUI) openModelPicker() error {
	if len(t.models) == 0 {
		t.chat.appendSystem(colorize("No models configured (providers[0].models)", ansiRed))
		return nil
	}
	labels := make([]string, len(t.models))
	initial := 0
	for i, m := range t.models {
		labels[i] = "  " + m
		if m == t.model {
			initial = i
		}
	}
	return t.openPicker(" Select model ", labels, t.models, initial, func(g *gocui.Gui, name string) error {
		return t.switchToModel(name)
	})
}

// handleThinking 处理 /thinking：带参数直接设置；无参数弹出选择列表。
// 与 /switch 一致：agent 忙碌时拒绝。
func (t *TUI) handleThinking(args string) error {
	if t.status.busy {
		t.chat.appendSystem("(still processing the previous message, please wait)")
		return nil
	}
	if t.agent == nil {
		t.chat.appendSystem(colorize("Error: /thinking is unavailable", ansiRed))
		return nil
	}
	if args == "" {
		return t.openThinkingPicker()
	}
	return t.setThinking(args)
}

// openThinkingPicker 弹出思考强度选择列表（/thinking 无参数时调用），
// 默认定位到当前值。
func (t *TUI) openThinkingPicker() error {
	labels := make([]string, len(thinkingEfforts))
	initial := 0
	for i, e := range thinkingEfforts {
		labels[i] = "  " + e
		if e == t.thinking {
			initial = i
		}
	}
	return t.openPicker(" Select thinking effort ", labels, thinkingEfforts, initial, func(g *gocui.Gui, name string) error {
		return t.setThinking(name)
	})
}

// setThinking 校验思考强度并应用（弹窗确认与 /thinking <name> 共用）。
// auto 归一化为不传参（agent 侧处理），其余值原样透传。
func (t *TUI) setThinking(name string) error {
	if !slices.Contains(thinkingEfforts, name) {
		t.chat.appendSystem(colorize("Unknown thinking effort: "+name+" (available: "+strings.Join(thinkingEfforts, ", ")+")", ansiRed))
		return nil
	}
	t.agent.SetThinking(name)
	t.thinking = name
	t.status.setThinking(name)
	t.chat.appendSystem("Thinking effort: " + name)
	return nil
}

// handleSessions 处理 /sessions（Ctrl+L 共用）：弹出最近会话列表，
// 每项显示标题与创建时间；Enter 切换到选中会话并重绘聊天区历史。
func (t *TUI) handleSessions() error {
	if t.status.busy {
		t.chat.appendSystem("(still processing the previous message, please wait)")
		return nil
	}
	if t.sessionItems == nil || t.switchSession == nil {
		t.chat.appendSystem(colorize("Error: /sessions is unavailable", ansiRed))
		return nil
	}
	items, err := t.sessionItems()
	if err != nil {
		t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		return nil
	}
	if len(items) == 0 {
		t.chat.appendSystem("No sessions yet")
		return nil
	}
	labels := make([]string, len(items))
	ids := make([]string, len(items))
	for i, it := range items {
		title := it.Title
		if title == "" {
			title = "(no title)"
		}
		title = truncateDisplay(title, 18) // 按显示宽度截断（CJK 计 2）
		labels[i] = "  " + padDisplay(title, 18) + " " + it.Time
		ids[i] = it.ID
	}
	return t.openPicker(" Select session ", labels, ids, 0, func(g *gocui.Gui, id string) error {
		return t.switchToSession(id)
	})
}

// handleSkills 列出已发现的 skills（/skills 命令，含来源层级与路径）。
// 与 /sessions 一致：仅展示，不打断进行中的生成，busy 时也可查看。
func (t *TUI) handleSkills() error {
	if t.skillsLister == nil {
		t.chat.appendSystem(colorize("Error: /skills is unavailable", ansiRed))
		return nil
	}
	items := t.skillsLister()
	if len(items) == 0 {
		t.chat.appendSystem("No skills found (place SKILL.md under ~/.zlite/skills/ or <cwd>/.zlite/skills/)")
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Available skills (%d):", len(items))
	for _, it := range items {
		fmt.Fprintf(&b, "\n  %s (%s): %s", it.Name, it.Source, it.Description)
		if it.Path != "" {
			fmt.Fprintf(&b, "\n    %s", it.Path)
		}
	}
	t.chat.appendSystem(b.String())
	return nil
}

// switchToSession 切换到指定会话并刷新聊天区为新会话的历史。
func (t *TUI) switchToSession(id string) error {
	if err := t.switchSession(id); err != nil {
		t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		return nil
	}
	if t.agent != nil {
		t.chat.loadHistory(t.agent.History())
	}
	t.chat.appendSystem("Switched to session " + id)
	return nil
}

// openSessionsKey 是 Ctrl+L 键位：与 /sessions 命令等效。
func (t *TUI) openSessionsKey(g *gocui.Gui, v *gocui.View) error {
	return t.handleSessions()
}

// newChatKey 是 Ctrl+N 键位：与 /new 命令等效（复用 busy 拒绝等逻辑）。
func (t *TUI) newChatKey(g *gocui.Gui, v *gocui.View) error {
	return t.newChat()
}

// switchModelKey 是 Shift+Tab 键位：与 /switch（无参数）等效。
// 走 handleSwitch 以复用 busy 检查与注入检查。
func (t *TUI) switchModelKey(g *gocui.Gui, v *gocui.View) error {
	return t.handleSwitch("")
}

// thinkingKey 是 Ctrl+T 键位：与 /thinking（无参数）等效。
// 走 handleThinking 以复用 busy 检查与注入检查。
func (t *TUI) thinkingKey(g *gocui.Gui, v *gocui.View) error {
	return t.handleThinking("")
}

// padDisplay 按终端显示宽度（displayWidth）把 s 右侧补空格到宽度 w；
// 已超宽时原样返回。用于会话列表标题列对齐。
func padDisplay(s string, w int) string {
	width := displayWidth(s)
	if width >= w {
		return s
	}
	return s + strings.Repeat(" ", w-width)
}

// displayWidth 返回字符串的终端显示宽度：CJK 等宽字符计 2，其余计 1。
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		if r > 0x2E7F { // CJK 部首区起视为宽字符（含中文、全角标点、假名）
			width += 2
		} else {
			width++
		}
	}
	return width
}

// truncateDisplay 按显示宽度截断 s（超出部分以省略号结尾）。
func truncateDisplay(s string, w int) string {
	width := 0
	for i, r := range []rune(s) {
		rw := 1
		if r > 0x2E7F {
			rw = 2
		}
		if width+rw > w {
			return string([]rune(s)[:i]) + "…"
		}
		width += rw
	}
	return s
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
	t.status.setBusy(true)   // 同步置位（同 submit：消除 busy 检查的 TOCTOU 窗口）
	t.chat.startProcessing() // 同步显示 [processing...]（/init 完成由 runInit 置 done）
	go t.runInit(msg)
	return nil
}

// runInit 在后台执行 init 任务（busy 已由 initProject 同步置位）。
func (t *TUI) runInit(msg string) {
	err := t.agent.RunInit(t.ctx, msg)
	t.ui(func() {
		t.status.setBusy(false)
		t.chat.finishProcessing()
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
		case agent.ThinkingStartEvent:
			// 后端返回思维链：processing → thinking
			t.ui(func() { t.chat.confirmThinking() })
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
  /init     Analyze the project and generate/refresh AGENTS.md
  /new      Start a new session (Ctrl+N; mode resets to plan)
  /plan     Switch to plan mode (read-only: inspect and search only)
  /build    Switch to build mode (writable: modify files, run commands)
  /switch   Switch model (Shift+Tab, pick from the list, or /switch <model>)
  /thinking Switch thinking effort (Ctrl+T, pick from the list, or /thinking <effort>; auto = let the API decide)
  /sessions Switch to a recent session (Ctrl+L)
  /skills   List discovered skills (global + project)
  /exit     Quit zlite
  /help     Show this help

Usage: type a message and press Enter to send; Tab toggles plan/build mode; Ctrl+C to quit.
Scroll chat history with PgUp/PgDn (page) or Home/End (top/bottom). Mouse selection is left to the terminal.`

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
