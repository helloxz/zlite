// Package tui 实现 zlite 的终端界面（bubbletea）。
//
// TUI 只消费 agent 的事件流并触发 agent.Run，不包含任何业务逻辑；
// 二期 ACP 接入时通过同一事件契约复用 agent 核心。
//
// 架构（bubbletea Elm 模型）：
//   - model（model.go）持有 *TUI 引用，Update 消息循环串行处理事件；
//   - chatView/statusView 只维护状态并生成渲染字符串，不直接画屏；
//   - agent 事件流经订阅 Cmd（waitAgentEvent）注入消息循环；
//   - 外部注入 API（SetAgent/SetModel/...）只改 TUI 字段，下次渲染自然生效。
package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/llm"
)

// agentFace 是 TUI 依赖的 agent 能力（测试可注入 fake）。
type agentFace interface {
	Events() <-chan agent.Event
	Run(ctx context.Context, msg string) error
	RunInit(ctx context.Context, msg string) error
	Compress(ctx context.Context) error
	Turns() int
	MaxTurns() int
	SetMode(m agent.Mode)
	Mode() agent.Mode
	SetThinking(t string)
	History() []llm.Message // 当前会话历史（/sessions 切换后渲染用）
}

// errQuit 是内部退出信号（/exit 命令用；Ctrl+C 直接走 tea.Quit）。
var errQuit = errors.New("quit")

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

	// models 是可选模型引用列表（/switch 用，来自配置全部渠道的
	// "provider_name/model_name" 扁平列表）；
	// switchModel 执行实际切换（app.go 注入：重建模型流并注入 agent）。
	models      []string
	switchModel func(name string) error

	// sessionItems 返回可切换的会话列表（/sessions 用，app.go 注入：
	// 最近 20 条，含标题与创建时间）；switchSession 执行会话切换并返回
	// 恢复后的模型引用（TUI 据此刷新状态栏）。
	sessionItems  func() ([]SessionItem, error)
	switchSession func(id string) (string, error)

	// skillsLister 返回已发现的 skills 列表（/skills 用，app.go 注入）。
	skillsLister func() []SkillItem

	// thinking 是当前思考强度显示值（默认 auto；与 agent 侧实际传参
	// 对应：auto 归一化为不传）。
	thinking string

	// setupNeeded 为 true 时进入首次配置引导（type → base_url → api_key → models），
	// 完成后调用 onSetupDone 落盘并热重载（app.go 注入）。
	setupNeeded bool
	onSetupDone func(config.SetupInput) error
	setupState  setupState
	setupInput  config.SetupInput

	chat   *chatView
	status *statusView

	// picker 非 nil 时表示列表选择弹窗打开（模态，仅消息循环线程读写）。
	picker *picker

	// screenH/screenW 是最近一次窗口尺寸（弹窗布局用；由 model.handleResize 同步）。
	screenH int
	screenW int

	// approvalCh 非 nil 时表示有正在等待用户确认的危险操作
	// （仅消息循环线程读写：Approver 经 program.Send 设置，submit 消费）。
	approvalCh chan agent.ApprovalDecision

	ctx     context.Context
	cancel  context.CancelFunc
	program *tea.Program // Run 启动后非 nil；Stop 与 Approver 经它投递
}

// New 创建 TUI。newSession 为 /new 命令的会话切换回调（可为 nil）。
func New(cfg *config.Config, a agentFace, model, cwd string, newSession func() error) *TUI {
	ctx, cancel := context.WithCancel(context.Background())
	chat := newChatView()
	chat.appendSystem("zlite ready - " + cwd)
	return &TUI{
		cfg: cfg, agent: a, model: model, cwd: cwd,
		newSession: newSession,
		thinking:   "auto",
		chat:       chat,
		status:     newStatusView(model, cwd),
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

// SetAgentInLoop 在消息循环（Update）内设置 agent——引导回调（EnableSetup
// 的 onSetupDone）在 Update 线程内同步执行，只能用它而不是 SetAgent：
// Program.Send 是同步投递（阻塞到消息被处理），从消息循环内调用会自死锁
// （实测首次引导最后一步回车后 UI 完全卡死、Ctrl+C 无效）。
// 订阅重连由引导完成路径返回 waitAgentEvent cmd（见 handleSetup）。
func (t *TUI) SetAgentInLoop(a agentFace) {
	t.agent = a
}

// SetModelInLoop 在消息循环内更新模型名（引导回调用）：不投递 refreshMsg
// （同 SetAgentInLoop 的死锁原因）；重绘由引导完成路径统一触发。
func (t *TUI) SetModelInLoop(name string) {
	t.setModel(name)
}

// SetAgent 替换 agent（外部线程调用；消息循环内请用 SetAgentInLoop）。
func (t *TUI) SetAgent(a agentFace) {
	t.agent = a
	// 事件流订阅可能已退出（agent 重建）：重新发起订阅并触发重绘
	if t.program != nil {
		t.program.Send(agentResubscribeMsg{})
	}
}

// SetModel 更新状态栏模型名（引导完成热重载后调用，外部线程）。
func (t *TUI) SetModel(name string) {
	t.setModel(name)
	if t.program != nil {
		t.program.Send(refreshMsg{})
	}
}

// setModel 是消息循环内的模型名更新：只改共享状态，不发 refreshMsg。
// 注意：bubbletea 的 Program.Send 是同步投递（阻塞到消息被处理），
// 从消息循环内调用会自死锁（实测：/switch 确认时卡死）；渲染由
// 调用方在 Update 末尾统一 refreshChat 完成。
func (t *TUI) setModel(name string) {
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

// SetSwitchModel 配置 /switch 命令：models 为可选模型引用列表
// （"provider_name/model_name"，来自配置全部渠道，app.go 注入），
// fn 执行实际切换（按引用解析渠道并重建模型流）。
func (t *TUI) SetSwitchModel(models []string, fn func(name string) error) {
	t.models = models
	t.switchModel = fn
}

// SetSessionSwitcher 配置 /sessions 命令（Ctrl+L 共用）：
// items 返回可切换的会话列表（最近 20 条），fn 按会话 ID 执行切换
// 并返回恢复后的模型引用（会话记录模型可解析时恢复，否则保持当前）。
func (t *TUI) SetSessionSwitcher(items func() ([]SessionItem, error), fn func(id string) (string, error)) {
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

// Run 启动 TUI（阻塞直到退出）。
func (t *TUI) Run() error {
	// AltScreen：进入切换 alternate screen buffer，退出自动恢复（与 gocui 一致）。
	// 刻意不启用鼠标（tea.WithMouseCellMotion 不开）：请求鼠标事件会让终端
	// 模拟器放弃原生文本选择；滚动一律走键盘，与 opencode 等工具同策略。
	p := tea.NewProgram(newModel(t), tea.WithAltScreen())
	t.program = p
	_, err := p.Run()
	t.program = nil
	t.cancel() // 终止未完成的 agent 运行
	return err
}

// Stop 请求 TUI 退出（外部信号处理用）。program.Quit 线程安全。
func (t *TUI) Stop() {
	if t.program != nil {
		t.program.Quit()
	}
}

// ---- 斜杠命令处理（逻辑与 gocui 版一致，仅触发方式改为消息循环） ----

// handleCommand 处理斜杠命令（支持参数："/init <要求>"）。
// 返回 errQuit 表示退出请求。
func (t *TUI) handleCommand(cmd string) error {
	name, args := splitCommand(cmd)
	switch name {
	case "/exit", "/quit":
		return errQuit
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
	case "/compress":
		return t.compressConversation()
	case "/help":
		t.chat.appendSystem(helpText)
	default:
		t.chat.appendSystem("Unknown command: " + cmd + " (type /help for available commands)")
	}
	return nil
}

// handleSwitch 处理 /switch：带参数直接切换；无参数提示列表（M2 弹窗）。
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
		t.chat.appendSystem(colorize("No models configured (add [[providers]] with models to ~/.zlite/config.toml)", ansiRed))
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
	return t.openPicker(" Select model ", labels, t.models, initial, func(name string) error {
		return t.switchToModel(name)
	})
}

// handleThinking 处理 /thinking：带参数直接设置；无参数提示列表（M2 弹窗）。
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
	return t.openPicker(" Select thinking effort ", labels, thinkingEfforts, initial, func(name string) error {
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
	// 时间前置 + 固定 11 列（"01-02 15:04"），标题列右侧截断：
	// 时间列天然对齐（无需按显示宽度 pad），长标题不会挤动时间列。
	// 标题列宽动态：常规屏 30 列，窄屏随屏幕宽收窄（46 = 时间 11 + 间隔 2
	// + 弹窗留白 4 + 屏幕两侧边距 29）。
	titleW := 30
	if t.screenW > 0 {
		titleW = minInt(30, maxInt(8, t.screenW-46))
	}
	for i, it := range items {
		title := it.Title
		if title == "" {
			title = "(no title)"
		}
		labels[i] = "  " + it.Time + "  " + truncateDisplay(title, titleW)
		ids[i] = it.ID
	}
	return t.openPicker(" Select session ", labels, ids, 0, func(id string) error {
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
	model, err := t.switchSession(id)
	if err != nil {
		t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		return nil
	}
	if t.agent != nil {
		t.chat.loadHistory(t.agent.History())
		t.status.setTurns(t.agent.Turns(), t.agent.MaxTurns()) // 切换会话后同步轮次显示
	}
	t.setModel(model) // 会话记录的模型可能被恢复，状态栏同步（消息循环内，不发消息）
	t.chat.appendSystem("Switched to session " + id)
	return nil
}

// switchToModel 校验模型引用并执行切换（弹窗确认与 /switch <name> 共用）。
func (t *TUI) switchToModel(name string) error {
	if !slices.Contains(t.models, name) {
		t.chat.appendSystem(colorize("Unknown model: "+name+" (available: "+strings.Join(t.models, ", ")+")", ansiRed))
		return nil
	}
	if err := t.switchModel(name); err != nil {
		t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		return nil
	}
	t.setModel(name) // 消息循环内，不发 refreshMsg（防 Program.Send 自死锁）
	t.chat.appendSystem("Switched to model: " + name)
	return nil
}

// compressConversation 执行 /compress：请求 agent 对全量历史做一次总结并
// 注入后续上下文。与 /init 同模式：busy 置位后异步执行，完成后由
// agentDoneMsg 统一收尾（busy 复位 + [done] + 错误展示）。
func (t *TUI) compressConversation() error {
	if t.status.busy {
		t.chat.appendSystem("(still processing the previous message, please wait)")
		return nil
	}
	t.chat.appendSystem("Compressing conversation: summarizing the full history...")
	t.status.setBusy(true)   // 同步置位（同 submit：消除 busy 检查的 TOCTOU 窗口）
	t.chat.startProcessing() // 同步显示 [processing...]（压缩完成由 runCompress 置 done）
	go t.runCompress()
	return nil
}

// runCompress 在后台执行压缩（busy 已由 compressConversation 同步置位）。
func (t *TUI) runCompress() {
	err := t.agent.Compress(t.ctx)
	if t.program != nil {
		t.program.Send(agentDoneMsg{err: err})
	}
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
	if t.program != nil {
		t.program.Send(agentDoneMsg{err: err})
	}
}

// runAgent 在后台执行一轮对话（busy 与用户消息已由 submit 同步处理）。
func (t *TUI) runAgent(msg string) {
	err := t.agent.Run(t.ctx, msg)
	if t.program != nil {
		t.program.Send(agentDoneMsg{err: err})
	}
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
	if t.agent != nil {
		t.status.setTurns(t.agent.Turns(), t.agent.MaxTurns()) // 新会话轮次归零
	}
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
func (t *TUI) toggleMode() {
	if t.agent.Mode() == agent.ModePlan {
		t.switchMode(agent.ModeBuild)
	} else {
		t.switchMode(agent.ModePlan)
	}
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
// 使用 x/ansi 的宽度计算（EAW ambiguous 按 1，与主流终端一致）。
func displayWidth(s string) int {
	return ansi.StringWidth(s)
}

// truncateDisplay 按显示宽度截断 s（超出部分以省略号结尾）。
// ANSI SGR 序列完整保留且不计宽（窄屏截断状态栏等含色文本时不会切坏序列）。
func truncateDisplay(s string, w int) string {
	runes := []rune(s)
	width := 0
	for i := 0; i < len(runes); {
		r := runes[i]
		if r == '\x1b' {
			// 跳过 ANSI 序列（CSI：ESC [ ... 字母；孤立 ESC 也跳过），不计宽
			if i+1 < len(runes) && runes[i+1] == '[' {
				i += 2
				for i < len(runes) {
					c := runes[i]
					i++
					if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '@' {
						break
					}
				}
				continue
			}
			i++
			continue
		}
		rw := 1
		if r > 0x2E7F { // CJK 部首区起视为宽字符（含中文、全角标点、假名）
			rw = 2
		}
		if width+rw > w {
			return string(runes[:i]) + "…"
		}
		width += rw
		i++
	}
	return s
}

// commandInfo 是斜杠命令的注册信息：/ 输入提示浮层与 /help 共用同一
// 数据源（避免列表漂移）。
type commandInfo struct {
	name string
	desc string
}

// commandInfos 是全部可用命令。注意 /exit 与 /quit 同义（都退出）。
var commandInfos = []commandInfo{
	{"/init", "Analyze the project and generate/refresh AGENTS.md"},
	{"/compress", "Summarize the full conversation once and inject it as context"},
	{"/new", "Start a new session (Ctrl+N; mode resets to plan)"},
	{"/plan", "Switch to plan mode (read-only: inspect and search only)"},
	{"/build", "Switch to build mode (writable: modify files, run commands)"},
	{"/switch", "Switch model (Shift+Tab, pick from the list, or /switch <model>)"},
	{"/thinking", "Switch thinking effort (Ctrl+T, pick from the list, or /thinking <effort>; auto = let the API decide)"},
	{"/sessions", "Switch to a recent session (Ctrl+L)"},
	{"/skills", "List discovered skills (global + project)"},
	{"/exit", "Quit zlite"},
	{"/help", "Show this help"},
}

// hintNameWidth 是 / 提示浮层中命令名的固定列宽（最长命令名 + 2 空格分隔），
// 保证所有命令的描述从同一列开始。
func hintNameWidth() int {
	w := 0
	for _, c := range commandInfos {
		if l := displayWidth(c.name); l > w {
			w = l
		}
	}
	return w + 2
}

// buildHelpText 从 commandInfos 生成 /help 文本（与 / 提示浮层同源）。
func buildHelpText() string {
	var b strings.Builder
	b.WriteString("Available commands:\n")
	for _, c := range commandInfos {
		b.WriteString("  " + c.name + "  " + c.desc + "\n")
	}
	b.WriteString("\nUsage: type a message and press Enter to send; Tab toggles plan/build mode; Ctrl+C to quit.\n")
	b.WriteString("Scroll chat history with PgUp/PgDn (page) or Home/End (top/bottom). Mouse selection is left to the terminal.")
	return b.String()
}

// helpText 是 /help 命令的输出（惰性生成，与 commandInfos 保持一致）。
var helpText = buildHelpText()

// ---- 首次配置引导（setup state machine）----

// startSetup 启动引导：输出欢迎语与第一步提问（Init 阶段投递的消息触发）。
func (t *TUI) startSetup() {
	t.setupState = setupType
	t.chat.appendSystem("Welcome to zlite! Let's configure your model provider.")
	t.chat.appendSystem("Type: enter 1 for openai.chat, 2 for openai.responses:")
}

// handleSetup 按当前步骤处理用户输入（submit 拦截）。
// api_key 必填（用户决策），其余非法输入提示重输；全部完成调用
// onSetupDone 落盘并热重载，失败则引导从头再来。
// 返回 (cmd, error)：引导完成且 agent 已注入时返回 waitAgentEvent cmd
// （agent 由 onSetupDone 经 SetAgentInLoop 注入，此前从未订阅过）。
func (t *TUI) handleSetup(msg string) (tea.Cmd, error) {
	switch t.setupState {
	case setupType:
		switch msg {
		case "1":
			t.setupInput.Type = config.TypeOpenAIChat
		case "2":
			t.setupInput.Type = config.TypeOpenAIResponses
		default:
			t.chat.appendSystem("Invalid choice. Enter 1 (openai.chat) or 2 (openai.responses):")
			return nil, nil
		}
		t.setupState = setupBaseURL
		t.chat.appendSystem("Base URL (e.g. https://api.example.com/v1):")
	case setupBaseURL:
		if msg == "" {
			t.chat.appendSystem("Base URL cannot be empty. Enter the endpoint base URL:")
			return nil, nil
		}
		t.setupInput.BaseURL = msg
		t.setupState = setupAPIKey
		t.chat.appendSystem("API key (saved to ~/.zlite/.env as ZLITE_DEFAULT_API_KEY):")
	case setupAPIKey:
		if msg == "" {
			t.chat.appendSystem("API key cannot be empty. Enter your API key:")
			return nil, nil
		}
		t.setupInput.APIKey = msg
		t.setupState = setupModels
		t.chat.appendSystem("Model(s), comma separated (e.g. gpt-4o, gpt-4o-mini):")
	case setupModels:
		models := config.SplitModels(msg)
		if len(models) == 0 {
			t.chat.appendSystem("At least one model is required. Enter model(s), comma separated:")
			return nil, nil
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
			return nil, nil
		}
		t.chat.appendSystem(colorize("Configuration saved. Happy coding!", ansiGreen))
		// 引导完成：agent 刚经 SetAgentInLoop 注入，启动事件订阅
		if t.agent != nil {
			return waitAgentEvent(t.agent.Events(), t.ctx.Done()), nil
		}
		return nil, nil
	}
	return nil, nil
}

// handleApproval 处理确认输入（y 批准 / n 拒绝）。
func (t *TUI) handleApproval(msg string) {
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
}
