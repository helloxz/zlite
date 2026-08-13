// Package tui 实现 zlite 的终端界面（gocui）。
//
// TUI 只消费 agent 的事件流并触发 agent.Run，不包含任何业务逻辑；
// 二期 ACP 接入时通过同一事件契约复用 agent 核心。
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/awesome-gocui/gocui"
	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/config"
)

// agentFace 是 TUI 依赖的 agent 能力（测试可注入 fake）。
type agentFace interface {
	Events() <-chan agent.Event
	Run(ctx context.Context, msg string) error
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
	agent agentFace
	model string
	cwd   string

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

// New 创建 TUI。
func New(cfg *config.Config, a agentFace, model, cwd string) *TUI {
	ctx, cancel := context.WithCancel(context.Background())
	return &TUI{
		cfg: cfg, agent: a, model: model, cwd: cwd,
		uiTasks: make(chan func(), 128),
		ctx:     ctx, cancel: cancel,
	}
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
	go t.consumeEvents()

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
	if strings.HasPrefix(msg, "/") {
		return t.handleCommand(msg)
	}
	if t.status.busy {
		t.chat.appendSystem("(still processing the previous message, please wait)")
		return nil
	}
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

// runAgent 在后台执行一轮对话。
func (t *TUI) runAgent(msg string) {
	t.ui(func() {
		t.chat.appendUser(msg)
		t.status.setBusy(true)
	})
	err := t.agent.Run(t.ctx, msg)
	t.ui(func() {
		t.status.setBusy(false)
		if err != nil {
			t.chat.appendSystem(colorize("Error: "+err.Error(), ansiRed))
		}
	})
}

// handleCommand 处理斜杠命令。
func (t *TUI) handleCommand(cmd string) error {
	switch cmd {
	case "/exit", "/quit":
		return gocui.ErrQuit
	case "/plan":
		t.switchMode(agent.ModePlan)
	case "/build":
		t.switchMode(agent.ModeBuild)
	case "/help":
		t.chat.appendSystem(helpText)
	default:
		t.chat.appendSystem("Unknown command: " + cmd + " (type /help for available commands)")
	}
	return nil
}

// switchMode 切换模式并提示（Tab 与 /plan /build 共用）。
func (t *TUI) switchMode(m agent.Mode) {
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
  /plan    Switch to plan mode (read-only: inspect and search only)
  /build   Switch to build mode (writable: modify files, run commands)
  /exit    Quit zlite
  /help    Show this help

Usage: type a message and press Enter to send; Tab toggles plan/build mode; Ctrl+C to quit.`
