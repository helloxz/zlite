package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/helloxz/zlite/internal/acp"
	"github.com/helloxz/zlite/internal/agent"
	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/llm"
	"github.com/helloxz/zlite/internal/mcp"
	"github.com/helloxz/zlite/internal/session"
	"github.com/helloxz/zlite/internal/skills"
	"github.com/helloxz/zlite/internal/tools"
	"github.com/helloxz/zlite/internal/tui"
)

// options 是命令行选项（main 解析 flag 后传入，便于测试）。
type options struct {
	mode string // -m
	cont bool   // -c
	list bool   // -l
	acp  bool   // --acp / acp 子命令：ACP 协议模式
}

// runtime 是一次运行所需的组件集合（run 与引导热重载共用）。
type runtime struct {
	cfg       *config.Config
	p         *config.Provider // 默认渠道（默认模型引用解析出的渠道）
	mode      agent.Mode
	modelName string           // 当前模型引用 provider_name/model_name
	cwd       string
	mgr       *session.Manager
	reg       *tools.Registry
	mcpMgr    *mcp.Manager // nil 表示未启用 MCP
	sk        *skills.Manager
	sess      *session.Session
	ag        *agent.Agent
	apUI      *tui.Approver // 非 nil 表示 TUI 内联确认器
}

// run 组装并启动 zlite：config → llm → tools → session → agent → tui / acp。
// 配置缺失或需要引导时进入 TUI 引导流程（完成后热重载，不重启）；
// ACP 模式无 TUI，配置未就绪时直接报错退出。
func run(opts options) error {
	if opts.acp && (opts.cont || opts.list || opts.mode != "") {
		return errors.New("--acp cannot be combined with -c/-l/-m")
	}

	cfgPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if errors.Is(err, config.ErrConfigNotFound) {
		if opts.acp {
			return errors.New("config file not found; run zlite once to complete setup before using --acp")
		}
		return runWithSetup(cfgPath, opts)
	}
	if err != nil {
		return err
	}
	if cfg.NeedsSetup() {
		if opts.acp {
			return errors.New("zlite is not configured; run zlite once to complete setup before using --acp")
		}
		return runWithSetup(cfgPath, opts)
	}
	return runWithConfig(cfgPath, opts, cfg)
}

// buildRuntime 组装运行期组件（run 与引导热重载共用）。
// opts.list 时仅构建 mgr/cwd（不创建会话），由调用方打印列表。
func buildRuntime(cfgPath string, cfg *config.Config, opts options) (*runtime, error) {
	// 默认模型引用（provider_name/model_name）：agent.default_model 优先，
	// 缺省取第一个渠道的第一个模型；ResolveModelSpec 校验渠道与模型存在。
	spec, err := cfg.DefaultModelName()
	if err != nil {
		return nil, err
	}
	p, _, err := cfg.ResolveModelSpec(spec)
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
	m, err := llm.BuildModelSpec(cfg, spec)
	if err != nil {
		return nil, err
	}
	streamer := llm.Bind(m)

	// 工作目录与工具
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	// skills：全局 ~/.zlite/skills/ + 项目 <cwd>/.zlite/skills/（项目优先同名覆盖），
	// read_skill 工具供模型按需读取 skill 正文
	sk := skills.New(filepath.Join(filepath.Dir(cfgPath), "skills"), filepath.Join(cwd, ".zlite", "skills"))
	reg := tools.New(cwd, tools.Options{
		ConfirmCommands:   cfg.Shell.ConfirmCommands,
		PlanExtraCommands: cfg.Shell.PlanExtraCommands,
		WebSearch:         cfg.Tools.WebSearch,
	})
	reg.Register(tools.ReadSkillTool(sk))

	// MCP：加载 ~/.zlite/mcp/ 配置（同步解析，快）并在后台连接（不阻塞启动）；
	// 工具在首轮对话前由 preRun（Attach）注册进注册表。
	var mcpMgr *mcp.Manager
	if cfg.MCP.Enabled {
		mcpMgr = mcp.New(cfg.MCP.Dir, cfg.MCP.MaxServers, cfg.MCP.MaxToolsPerServer)
		// 配置解析警告立即打印（TUI 尚未启动，stderr 安全）
		for _, w := range mcpMgr.ConfigWarnings() {
			fmt.Fprintln(os.Stderr, w)
		}
		if !opts.list {
			mcpMgr.ConnectAsync() // 后台连接：本地 stdio 毫秒级，远程慢 server 不阻塞启动
		}
	}

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

	rt := &runtime{cfg: cfg, p: p, mode: mode, modelName: spec, cwd: cwd, mgr: mgr, reg: reg, mcpMgr: mcpMgr, sk: sk, apUI: apUI}
	if opts.list || opts.acp {
		return rt, nil // 列表模式不创建会话；ACP 模式会话按 NewSession 请求创建
	}

	var sess *session.Session
	if opts.cont {
		sess, err = mgr.Continue(cwd)
		if errors.Is(err, session.ErrNoSession) {
			return nil, fmt.Errorf("当前目录没有可继续的会话（直接运行 zlite 开始新会话）")
		}
	} else {
		sess, err = mgr.Create(cwd, p.Name, spec, string(mode))
	}
	if err != nil {
		return nil, err
	}
	rt.sess = sess
	rt.ag = agent.New(cfg, streamer, reg, sess, approver, cwd, mode, sk)
	// MCP 工具注册：首轮对话前确保连接完成（Attach 幂等，每轮零开销），
	// 连接警告经 agent 事件广播（TUI 展示为 system 消息）。
	if mcpMgr != nil {
		rt.ag.SetPreRun(func(ctx context.Context) error {
			if err := mcpMgr.Attach(ctx, reg); err != nil {
				return err
			}
			for _, w := range mcpMgr.TakeWarnings() {
				rt.ag.Notify(w)
			}
			return nil
		})
	}
	return rt, nil
}

// switchModel 切换当前模型（/switch 命令用）：按模型引用 "provider_name/model_name"
// 重建模型并注入 agent，更新 modelName，并在会话中留痕。name 须来自配置的
// 模型列表（TUI 已校验）。调用方须保证 agent 空闲（TUI 在 busy 时拒绝 /switch）。
func (rt *runtime) switchModel(spec string) error {
	m, err := llm.BuildModelSpec(rt.cfg, spec)
	if err != nil {
		return err
	}
	rt.ag.SetStreamer(llm.Bind(m))
	rt.modelName = spec
	_ = rt.sess.AppendMeta("model_change", spec) // 落盘留痕；meta 失败不影响切换
	return nil
}

// switchSession 切换到指定 ID 的会话（/sessions 命令用）：
// 打开会话文件并替换 agent 的当前会话（旧会话由 SetSession 关闭），
// 模式同步为会话创建时的模式（状态栏与工具集随之更新）；
// 返回恢复后的模型引用：会话记录的模型可解析则恢复（含旧格式兼容），
// 否则保持当前模型。
func (rt *runtime) switchSession(id string) (string, error) {
	if rt.sess != nil && rt.sess.ID == id {
		return rt.modelName, nil // 已在该会话，无需切换
	}
	sess, err := rt.mgr.Open(rt.cwd, id)
	if err != nil {
		return "", err
	}
	rt.sess = sess
	rt.ag.SetSession(sess)
	if m, err := agent.ParseMode(sess.Mode); err == nil {
		rt.ag.SetMode(m) // 广播 ModeChangeEvent，TUI 状态栏同步
	}
	// 恢复会话记录的模型引用（配置变更后可能失效；失败保持当前模型）
	if spec, err := rt.cfg.RestoreModelSpec(sess.Provider, sess.Model); err == nil && spec != rt.modelName {
		if m, err := llm.BuildModelSpec(rt.cfg, spec); err == nil {
			rt.ag.SetStreamer(llm.Bind(m))
			rt.modelName = spec
		}
	}
	return rt.modelName, nil
}

// sessionItems 返回最近 20 条会话（/sessions 列表数据源）。
// 展示前先清理空会话（创建后未对话即退出的残留），再取列表。
func (rt *runtime) sessionItems() ([]tui.SessionItem, error) {
	skipID := ""
	if rt.sess != nil {
		skipID = rt.sess.ID // 当前打开的会话不删（文件句柄仍在使用）
	}
	if _, err := rt.mgr.PruneEmpty(rt.cwd, 30, skipID); err != nil {
		return nil, err
	}
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

// skillItems 返回 /skills 列表数据源（skills.SkillInfo → tui.SkillItem）。
func (rt *runtime) skillItems() []tui.SkillItem {
	infos := rt.sk.List()
	items := make([]tui.SkillItem, 0, len(infos))
	for _, in := range infos {
		items = append(items, tui.SkillItem{Name: in.Name, Description: in.Description, Source: in.Source, Path: in.Path})
	}
	return items
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
	// MCP 连接在两种模式下都要收尾（ACP 分支在 runACP 内阻塞至断开）
	if rt.mcpMgr != nil {
		defer rt.mcpMgr.Close() // 关闭 MCP 连接（终止 stdio 子进程）
	}
	if opts.acp {
		return runACP(rt)
	}
	defer rt.sess.Close()
	return startTUI(rt, opts)
}

// runACP 以 ACP 协议模式运行：Agent 侧 stdio 连接，阻塞至客户端断开。
// 注意：ACP 通信独占 stdout，任何日志只能走 stderr（上层已保证）。
func runACP(rt *runtime) error {
	ap := acp.New(acp.Options{
		Cfg:             rt.cfg,
		ModelName:       rt.modelName, // 默认模型引用 provider_name/model_name
		Cwd:             rt.cwd,
		Mgr:             rt.mgr,
		GlobalSkillsDir: rt.sk.GlobalDir(),
		AutoApprove:     rt.cfg.Agent.AutoApprove,
		MCP:             rt.mcpMgr, // nil 时不启用 MCP（每个 ACP 会话注册到自己的注册表）
	})
	conn := acpsdk.NewAgentSideConnection(ap, os.Stdout, os.Stdin)
	ap.Attach(conn)
	<-conn.Done()
	return nil
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
		// 3. 热重载进现有 TUI（不重启进程）。
		// 注意：onSetupDone 在 tea 消息循环（Update）内同步执行，
		// 这里只能用 InLoop 版 setter——SetAgent/SetModel 内部
		// Program.Send 是同步投递，从消息循环内调用会自死锁
		// （实测首次引导最后一步回车后 UI 卡死、Ctrl+C 无效）。
		t.SetAgentInLoop(rt.ag)
		t.SetModelInLoop(rt.modelName)
		t.SetSwitchModel(rt.cfg.AllModels(), rt.switchModel)
		t.SetSessionSwitcher(rt.sessionItems, rt.switchSession)
		t.SetSkillsLister(rt.skillItems)
		t.SetNewSession(func() error {
			ns, err := rt.mgr.Create(rt.cwd, rt.p.Name, rt.modelName, string(agent.ModePlan))
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
		ns, err := rt.mgr.Create(rt.cwd, rt.p.Name, rt.modelName, string(agent.ModePlan))
		if err != nil {
			return err
		}
		rt.sess = ns
		rt.ag.SetSession(ns)
		return nil
	}
	t := tui.New(rt.cfg, rt.ag, rt.modelName, rt.cwd, newSession)
	t.SetSwitchModel(rt.cfg.AllModels(), rt.switchModel)
	t.SetSessionSwitcher(rt.sessionItems, rt.switchSession)
	t.SetSkillsLister(rt.skillItems)
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
	fmt.Printf("%-19s  %-6s  %-24s  %-22s  %s\n", "ID", "MODE", "MODEL", "TITLE", "消息数")
	for _, info := range infos {
		model := info.Model
		if len(model) > 24 { // 模型引用 provider_name/model_name 可能较长，截断展示
			model = model[:24]
		}
		title := info.Title
		if title == "" {
			title = "(no title)" // 旧会话无标题记录
		}
		if tr := []rune(title); len(tr) > 20 {
			title = string(tr[:20]) + "…"
		}
		fmt.Printf("%-19s  %-6s  %-24s  %-22s  %d\n", info.ID, info.Mode, model, title, info.Messages)
	}
	return nil
}
