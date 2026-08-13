// Package agent 是 zlite 的核心对话循环，与 UI 完全解耦。
//
// Agent 消费用户消息，驱动模型生成（llm.Streamer）、工具调度（tools.Registry）、
// 权限确认（Approver）与会话落盘（session.Session），并以事件流（Events）
// 向外部（TUI / ACP）广播进度。
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/llm"
	"github.com/helloxz/zlite/internal/session"
	"github.com/helloxz/zlite/internal/tools"
	"github.com/zendev-sh/goai"
)

// Agent 是一轮轮对话的执行者。
type Agent struct {
	cfg      *config.Config
	mu       sync.Mutex // 保护 streamer（/switch 与 runOnce 并发替换/读取）
	streamer llm.Streamer
	registry *tools.Registry
	sess     *session.Session
	approver Approver
	cwd      string
	mode     Mode
	// thinking 是思考强度（none/low/medium/high/xhigh/max），
	// 空字符串表示 auto：不传 reasoning_effort，由 API 自行决定。
	thinking string

	events chan Event
}

// New 创建 Agent。
// mode 为初始模式（plan/build）。
func New(cfg *config.Config, streamer llm.Streamer, registry *tools.Registry, sess *session.Session, approver Approver, cwd string, mode Mode) *Agent {
	return &Agent{
		cfg:      cfg,
		streamer: streamer,
		registry: registry,
		sess:     sess,
		approver: approver,
		cwd:      cwd,
		mode:     mode,
		events:   make(chan Event, 64),
	}
}

// Events 返回事件流（只读）。
func (a *Agent) Events() <-chan Event { return a.events }

// Mode 返回当前模式。
func (a *Agent) Mode() Mode { return a.mode }

// SetSession 切换当前会话（/new 命令用）：关闭旧会话句柄并替换为新会话。
// 会话文件实时落盘，切换安全；新会话历史为空。
func (a *Agent) SetSession(sess *session.Session) {
	if a.sess != nil {
		a.sess.Close()
	}
	a.sess = sess
}

// SetStreamer 替换底层模型流（/switch 命令用）。
// 加锁保证与 runOnce 的并发读取安全；正在进行的生成继续使用旧模型流，
// 替换从下一轮生成开始生效。
func (a *Agent) SetStreamer(s llm.Streamer) {
	a.mu.Lock()
	a.streamer = s
	a.mu.Unlock()
}

// SetThinking 设置思考强度（/thinking 命令用）。
// "auto" 归一化为空字符串：不传 reasoning_effort 参数，由 API 自行决定；
// 其余值原样透传（是否支持由后端决定，错误会经流返回）。
// 与 mode 一致：仅主循环线程修改、busy 时拒绝切换，无需加锁。
func (a *Agent) SetThinking(t string) {
	if t == "auto" {
		t = ""
	}
	if a.thinking == t {
		return
	}
	a.thinking = t
	if t == "" {
		t = "auto"
	}
	a.sess.AppendMeta("thinking_change", t)
}

// SetMode 切换模式并广播事件。
func (a *Agent) SetMode(m Mode) {
	if a.mode == m {
		return
	}
	a.mode = m
	a.sess.AppendMeta("mode_change", string(m))
	a.emit(ModeChangeEvent{Mode: m})
}

// History 返回当前会话的模型消息历史（/sessions 切换会话后 UI 渲染用）。
func (a *Agent) History() []llm.Message {
	return a.sess.ToMessages()
}

// Run 执行一轮对话：追加用户消息 → 模型生成（含工具循环）→ 落盘。
func (a *Agent) Run(ctx context.Context, userMsg string) error {
	if strings.TrimSpace(userMsg) == "" {
		return errors.New("消息不能为空")
	}
	if err := a.sess.AppendUser(userMsg); err != nil {
		return fmt.Errorf("保存用户消息失败: %w", err)
	}
	return a.runOnce(ctx, a.buildPrompt())
}

// RunInit 执行项目初始化任务（/init）：扫描项目并生成/更新 AGENTS.md。
// 复用 runOnce 核心循环，系统提示词替换为 init 指令模板。
func (a *Agent) RunInit(ctx context.Context, userMsg string) error {
	if err := a.sess.AppendUser(userMsg); err != nil {
		return fmt.Errorf("保存用户消息失败: %w", err)
	}
	return a.runOnce(ctx, initSystemPrompt(a.cwd, a.mode))
}

// buildPrompt 组装当前模式的系统提示词（含项目 AGENTS.md 上下文，每次实时读取：
// /init 或手改 AGENTS.md 后下一次对话立即生效，无需重启）。
func (a *Agent) buildPrompt() string {
	toolList := a.registry.ForMode(tools.Mode(a.mode))
	projectCtx := ""
	if a.cfg.Agent.LoadAgentsMD {
		projectCtx = loadProjectContext(a.cwd)
	}
	return buildSystemPrompt(a.cwd, a.mode, toolDescriptions(toolList), projectCtx)
}

// runOnce 是核心生成循环（Run/RunInit 共用）：截断 → 组装请求 → StreamText
// → 事件 → 落盘。用户消息须已由调用方 AppendUser。
func (a *Agent) runOnce(ctx context.Context, system string) error {
	// 上下文截断
	all := a.sess.ToMessages()
	history := truncateMessages(all, defaultMaxHistoryMessages)
	if len(history) != len(all) {
		a.sess.AppendMeta("context_truncated", truncationNote(len(all), len(history)))
	}

	// 组装请求
	toolList := a.registry.ForMode(tools.Mode(a.mode))

	// 锁内取当前模型流：/switch 可并发替换，正在进行的生成不受影响
	a.mu.Lock()
	streamer := a.streamer
	a.mu.Unlock()

	stream, err := streamer.StreamText(ctx, llm.StreamRequest{
		System:          system,
		Messages:        history,
		Tools:           toolList,
		MaxSteps:        a.cfg.Agent.MaxSteps,
		ReasoningEffort: a.thinking,
		Hooks: llm.Hooks{
			BeforeToolExecute: a.onBeforeToolExecute,
			ToolResult:        a.onToolResult,
		},
	})
	if err != nil {
		return fmt.Errorf("模型调用失败: %w", err)
	}

	// 消费流
	var fullText strings.Builder
	var usage llm.Usage
	for ch := range stream.Chunks() {
		switch {
		case ch.Err != nil:
			return ch.Err
		case ch.Text != "":
			fullText.WriteString(ch.Text)
			a.emit(TextDeltaEvent{Text: ch.Text})
		case ch.ToolCall != nil:
			a.onToolCall(*ch.ToolCall)
		case ch.Finish:
			usage = ch.Usage
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}

	a.emit(TextDoneEvent{FullText: fullText.String()})
	if err := a.sess.AppendAssistant(fullText.String(), &usage); err != nil {
		return fmt.Errorf("保存助手回复失败: %w", err)
	}
	a.emit(DoneEvent{Usage: usage})
	return nil
}

// onToolCall 处理模型请求的工具调用：记录会话 + 广播事件。
func (a *Agent) onToolCall(tc llm.ToolCall) {
	a.sess.AppendToolCall(tc.ID, tc.Name, tc.Input)
	a.emit(ToolCallEvent{CallID: tc.ID, Name: tc.Name, Input: tc.Input})
}

// onBeforeToolExecute 是工具执行前的权限确认拦截点。
// 拒绝时工具不执行，拒绝原因作为工具结果返回给模型（模型可调整方案）。
func (a *Agent) onBeforeToolExecute(info llm.BeforeToolExecuteInfo) llm.BeforeToolExecuteResult {
	need, summary := a.registry.NeedApproveFor(info.Name, info.Input)
	if !need {
		return llm.BeforeToolExecuteResult{}
	}

	decision, err := a.approver.Request(info.Ctx, ApprovalRequest{
		CallID:  info.CallID,
		Tool:    info.Name,
		Summary: summary,
	})
	if err != nil {
		return llm.BeforeToolExecuteResult{
			Skip:   true,
			Result: "确认流程出错，操作已拒绝: " + err.Error(),
		}
	}
	if decision == Denied {
		return llm.BeforeToolExecuteResult{Skip: true, Result: "用户拒绝了该操作"}
	}
	return llm.BeforeToolExecuteResult{}
}

// onToolResult 处理工具执行结果：记录会话 + 广播事件。
func (a *Agent) onToolResult(info llm.ToolResultInfo) {
	a.sess.AppendToolResult(info.CallID, info.Name, info.Output, info.Error != nil, 0)
	a.emit(ToolResultEvent{
		CallID: info.CallID,
		Name:   info.Name,
		Output: info.Output,
		Error:  info.Error != nil,
	})
}

// toolDescriptions 提取工具描述列表（"name: description"，注入系统提示词）。
func toolDescriptions(ts []goai.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name+": "+t.Description)
	}
	return out
}

// emit 广播事件（channel 满时丢弃，避免阻塞生成循环；UI 消费慢时可接受）。
func (a *Agent) emit(e Event) {
	select {
	case a.events <- e:
	default:
	}
}
