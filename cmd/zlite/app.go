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

// run 组装并启动 zlite：config → llm → tools → session → agent → tui。
func run(opts options) error {
	// 1. 配置
	cfgPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if errors.Is(err, config.ErrConfigNotFound) {
		if werr := config.WriteTemplate(cfgPath); werr != nil {
			return werr
		}
		return fmt.Errorf("首次运行：已生成配置模板 %s\n请编辑后重新运行（api_key 可用环境变量 ${ZLITE_API_KEY} 引用）", cfgPath)
	}
	if err != nil {
		return err
	}

	p, err := cfg.DefaultProvider()
	if err != nil {
		return err
	}

	// 2. 模式（命令行覆盖配置文件）
	modeStr := cfg.Agent.Mode
	if opts.mode != "" {
		modeStr = opts.mode
	}
	mode, err := agent.ParseMode(modeStr)
	if err != nil {
		return err
	}

	// 3. 模型
	model, err := llm.BuildModel(p)
	if err != nil {
		return err
	}
	streamer := llm.Bind(model)

	// 4. 工作目录与工具
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	reg := tools.New(cwd, cfg.Shell.ConfirmCommands)

	// 5. 会话
	mgr := session.NewManager(filepath.Join(filepath.Dir(cfgPath), "sessions"))
	if opts.list {
		return listSessions(mgr, cwd)
	}

	var sess *session.Session
	if opts.cont {
		sess, err = mgr.Continue(cwd)
		if errors.Is(err, session.ErrNoSession) {
			return fmt.Errorf("当前目录没有可继续的会话（直接运行 zlite 开始新会话）")
		}
	} else {
		sess, err = mgr.Create(cwd, p, string(mode))
	}
	if err != nil {
		return err
	}
	defer sess.Close()

	// 6. agent + 确认器
	// auto_approve=true 时自动批准（信任模式）；否则危险命令经 TUI 内联确认。
	// 写文件工具（write_file/edit_file/delete）按用户决策直接执行，不经过确认。
	var approver agent.Approver
	if cfg.Agent.AutoApprove {
		approver = agent.NewApprover(true)
	} else {
		approver = &tui.Approver{}
	}
	ag := agent.New(cfg, streamer, reg, sess, approver, cwd, mode)

	// 7. TUI + 信号处理（SIGINT/SIGTERM 优雅退出，会话已实时落盘）
	// newSession：/new 命令的会话切换回调（创建新会话并切换；defer 的 Close 会
	// 关闭最终会话，旧会话由 agent.SetSession 关闭，Session.Close 幂等）。
	newSession := func() error {
		ns, err := mgr.Create(cwd, p, string(agent.ModePlan))
		if err != nil {
			return err
		}
		sess = ns
		ag.SetSession(ns)
		return nil
	}
	t := tui.New(cfg, ag, p.Models[0], cwd, newSession)
	if ap, ok := approver.(*tui.Approver); ok {
		ap.Attach(t) // agent 先于 TUI 创建，此处补绑确认器
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
	fmt.Printf("%-19s  %-6s  %-12s  %s\n", "ID", "MODE", "MODEL", "消息数")
	for _, info := range infos {
		model := info.Model
		if len(model) > 12 {
			model = model[:12]
		}
		fmt.Printf("%-19s  %-6s  %-12s  %d\n", info.ID, info.Mode, model, info.Messages)
	}
	return nil
}
