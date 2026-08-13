package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/llm"
	"github.com/helloxz/zlite/internal/session"
	"github.com/helloxz/zlite/internal/tools"
	"github.com/helloxz/zlite/internal/tui"
)

// options 是命令行选项（main 解析 flag 后传入，便于测试）。
type options struct {
	mode string // -m
	cont bool   // -c
	list bool   // -l
}

// runtime 是一次运行所需的组件集合（run 与引导热重载共用）。
type runtime struct {
	cfg       *config.Config
	p         *config.Provider
	mode      agent.Mode
	modelName string
	cwd       string
	mgr       *session.Manager
	reg       *tools.Registry
	sess      *session.Session
	ag        *agent.Agent
	apUI      *tui.Approver // 非 nil 表示 TUI 内联确认器
}

// run 组装并启动 zlite：config → llm → tools → session → agent → tui。
// 配置缺失或需要引导时进入 TUI 引导流程（完成后热重载，不重启）。
func run(opts options) error {
	cfgPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if errors.Is(err, config.ErrConfigNotFound) {
		return runWithSetup(cfgPath, opts)
	}
	if err != nil {
		return err
	}
	if cfg.NeedsSetup() {
		return runWithSetup(cfgPath, opts)
	}
	return runWithConfig(cfgPath, opts, cfg)
}

// buildRuntime 组装运行期组件（run 与引导热重载共用）。
// opts.list 时仅构建 mgr/cwd（不创建会话），由调用方打印列表。
func buildRuntime(cfgPath string, cfg *config.Config, opts options) (*runtime, error) {
	p, err := cfg.DefaultProvider()
	if err != nil {
		return nil, err
	}

	// 模式（命令行覆盖配置文件）
	modeStr := cfg.Agent.Mode
	if opts.mode != "" {
		modeStr = opts.mode
	}
	mode, err := agent.ParseMode(modeStr)
	if err != nil {
		return nil, err
	}

	// 模型
	m, err := llm.BuildModel(p)
	if err != nil {
		return nil, err
	}
	streamer := llm.Bind(m)

	// 工作目录与工具
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	reg := tools.New(cwd, cfg.Shell.ConfirmCommands)

	// 会话
	mgr := session.NewManager(filepath.Join(filepath.Dir(cfgPath), "sessions"))

	// 确认器：auto_approve=true 自动批准；否则危险命令经 TUI 内联确认。
	// 写文件工具（write_file/edit_file/delete）按用户决策直接执行，不经过确认。
	var approver agent.Approver
	var apUI *tui.Approver
	if cfg.Agent.AutoApprove {
		approver = agent.NewApprover(true)
	} else {
		apUI = &tui.Approver{}
		approver = apUI
	}

	rt := &runtime{cfg: cfg, p: p, mode: mode, modelName: p.Models[0], cwd: cwd, mgr: mgr, reg: reg, apUI: apUI}
	if opts.list {
		return rt, nil // 列表模式不创建会话
	}

	var sess *session.Session
	if opts.cont {
		sess, err = mgr.Continue(cwd)
		if errors.Is(err, session.ErrNoSession) {
			return nil, fmt.Errorf("当前目录没有可继续的会话（直接运行 zlite 开始新会话）")
		}
	} else {
		sess, err = mgr.Create(cwd, p, string(mode))
	}
	if err != nil {
		return nil, err
	}
	rt.sess = sess
	rt.ag = agent.New(cfg, streamer, reg, sess, approver, cwd, mode)
	return rt, nil
}

// switchModel 切换当前模型（/switch 命令用）：按名重建模型并注入 agent，
// 更新 modelName，并在会话中留痕。name 须来自配置的模型列表（TUI 已校验）。
// 调用方须保证 agent 空闲（TUI 在 busy 时拒绝 /switch）。
func (rt *runtime) switchModel(name string) error {
	m, err := llm.BuildModelNamed(rt.p, name)
	if err != nil {
		return err
	}
	rt.ag.SetStreamer(llm.Bind(m))
	rt.modelName = name
	_ = rt.sess.AppendMeta("model_change", name) // 落盘留痕；meta 失败不影响切换
	return nil
}

// switchSession 切换到指定 ID 的会话（/sessions 命令用）：
// 打开会话文件并替换 agent 的当前会话（旧会话由 SetSession 关闭），
// 模式同步为会话创建时的模式（状态栏与工具集随之更新）。
func (rt *runtime) switchSession(id string) error {
	if rt.sess != nil && rt.sess.ID == id {
		return nil // 已在该会话，无需切换
	}
	sess, err := rt.mgr.Open(rt.cwd, id)
	if err != nil {
		return err
	}
	rt.sess = sess
	rt.ag.SetSession(sess)
	if m, err := agent.ParseMode(sess.Mode); err == nil {
		rt.ag.SetMode(m) // 广播 ModeChangeEvent，TUI 状态栏同步
	}
	return nil
}

// sessionItems 返回最近 20 条会话（/sessions 列表数据源）。
func (rt *runtime) sessionItems() ([]tui.SessionItem, error) {
	infos, err := rt.mgr.List(rt.cwd)
	if err != nil {
		return nil, err
	}
	if len(infos) > 20 {
		infos = infos[:20] // List 已按创建时间倒序
	}
	items := make([]tui.SessionItem, 0, len(infos))
	for _, in := range infos {
		items = append(items, tui.SessionItem{
			ID:    in.ID,
			Title: in.Title,
			Time:  in.CreatedAt.Format("01-02 15:04"),
		})
	}
	return items, nil
}

// runWithConfig 正常启动（配置已就绪）。
func runWithConfig(cfgPath string, opts options, cfg *config.Config) error {
	rt, err := buildRuntime(cfgPath, cfg, opts)
	if err != nil {
		return err
	}
	if opts.list {
		return listSessions(rt.mgr, rt.cwd)
	}
	defer rt.sess.Close()
	return startTUI(rt, opts)
}

// runWithSetup 首次运行或缺配置：进入 TUI 引导，完成后热重载直接进入对话。
func runWithSetup(cfgPath string, opts options) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	t := tui.New(nil, nil, "", cwd, nil)
	t.EnableSetup(func(in config.SetupInput) error {
		// 1. 落盘（.env 存密钥，config.toml 只更新 providers 段）
		if err := config.ApplySetup(cfgPath, in); err != nil {
			return err
		}
		// 2. 重新加载配置并完整重建运行期组件
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		rt, err := buildRuntime(cfgPath, cfg, opts)
		if err != nil {
			return err
		}
		// 3. 热重载进现有 TUI（不重启进程）
		t.SetAgent(rt.ag)
		t.SetModel(rt.modelName)
		t.SetSwitchModel(rt.p.Models, rt.switchModel)
		t.SetSessionSwitcher(rt.sessionItems, rt.switchSession)
		t.SetNewSession(func() error {
			ns, err := rt.mgr.Create(rt.cwd, rt.p, string(agent.ModePlan))
			if err != nil {
				return err
			}
			rt.sess = ns
			rt.ag.SetSession(ns)
			return nil
		})
		if rt.apUI != nil {
			rt.apUI.Attach(t) // agent 晚于 TUI 创建，此处补绑确认器
		}
		return nil
	})
	return t.Run()
}

// startTUI 组装 TUI 并启动主循环（含 SIGINT/SIGTERM 优雅退出，会话已实时落盘）。
func startTUI(rt *runtime, opts options) error {
	// newSession：/new 命令的会话切换回调（创建新会话并切换；defer 的 Close 会
	// 关闭最终会话，旧会话由 agent.SetSession 关闭，Session.Close 幂等）。
	newSession := func() error {
		ns, err := rt.mgr.Create(rt.cwd, rt.p, string(agent.ModePlan))
		if err != nil {
			return err
		}
		rt.sess = ns
		rt.ag.SetSession(ns)
		return nil
	}
	t := tui.New(rt.cfg, rt.ag, rt.modelName, rt.cwd, newSession)
	t.SetSwitchModel(rt.p.Models, rt.switchModel)
	t.SetSessionSwitcher(rt.sessionItems, rt.switchSession)
	if rt.apUI != nil {
		rt.apUI.Attach(t) // agent 先于 TUI 创建，此处补绑确认器
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		t.Stop()
	}()

	return t.Run()
}

// listSessions 打印当前目录的会话列表。
func listSessions(mgr *session.Manager, cwd string) error {
	infos, err := mgr.List(cwd)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		fmt.Println("当前目录没有会话记录")
		return nil
	}
	fmt.Printf("%-19s  %-6s  %-12s  %-22s  %s\n", "ID", "MODE", "MODEL", "TITLE", "消息数")
	for _, info := range infos {
		model := info.Model
		if len(model) > 12 {
			model = model[:12]
		}
		title := info.Title
		if title == "" {
			title = "(no title)" // 旧会话无标题记录
		}
		if tr := []rune(title); len(tr) > 20 {
			title = string(tr[:20]) + "…"
		}
		fmt.Printf("%-19s  %-6s  %-12s  %-22s  %d\n", info.ID, info.Mode, model, title, info.Messages)
	}
	return nil
}
