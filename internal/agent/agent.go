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
	"time"

	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/llm"
	"github.com/helloxz/zlite/internal/session"
	"github.com/helloxz/zlite/internal/skills"
	"github.com/helloxz/zlite/internal/tools"
	"github.com/zendev-sh/goai"
)

// SkillsProvider 提供已发现的 skills 列表（*skills.Manager 实现；nil 表示未启用）。
// 定义为导出接口，便于调用方（cmd / acp）安全传 nil。
type SkillsProvider interface {
	List() []skills.SkillInfo
}

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
	skills   SkillsProvider
	// thinking 是思考强度（none/low/medium/high/xhigh/max），
	// 空字符串表示 auto：不传 reasoning_effort，由 API 自行决定。
	thinking string

	events chan Event
	// pendingImages 是当轮外部注入的图片（如 ACP 协议的 image content block：
	// 无本地文件、不持久化，当轮有效）：RunWithImages 设置，runOnce 的
	// buildHistory 消费后清空。调用方保证同步串行（TUI busy 锁 / ACP
	// waitIdle），无需额外加锁。
	pendingImages []llm.Image
	// preRun 是每轮生成开始前的回调（如 MCP 连接与工具注册）。
	// 由调用方注入；应自行保证幂等（每轮都会调用）；nil 表示跳过。
	preRun func(ctx context.Context) error
	// approvalMu 串行化权限确认：goai 对并行工具调用（executeToolsParallel）
	// 并发触发多个 onBeforeToolExecute，而确认器实现（如 TUI 单通道弹窗）
	// 通常只支持一个挂起请求——并发会互相覆盖导致永久阻塞。
	// 加锁后一次只弹一个确认，其余工具调用等待。
	approvalMu sync.Mutex
	// titleGen 是标题生成的 Streamer 覆盖（测试注入用；nil 时按 cfg.DefaultModelName 构建）。
	titleGen llm.Streamer
}
// New 创建 Agent。
// mode 为初始模式（plan/build）；skills 提供 skills 列表（nil 时不注入，测试可传 nil）。
func New(cfg *config.Config, streamer llm.Streamer, registry *tools.Registry, sess *session.Session, approver Approver, cwd string, mode Mode, skills SkillsProvider) *Agent {
	return &Agent{
		cfg:      cfg,
		streamer: streamer,
		registry: registry,
		sess:     sess,
		approver: approver,
		cwd:      cwd,
		mode:     mode,
		skills:   skills,
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

// Thinking 返回当前思考强度（"auto" 表示不传参；与 SetThinking 归一化规则对应）。
func (a *Agent) Thinking() string {
	if a.thinking == "" {
		return "auto"
	}
	return a.thinking
}

// SetTitleGenStreamer 注入标题生成的 Streamer（测试用；nil 恢复默认行为）。
func (a *Agent) SetTitleGenStreamer(s llm.Streamer) {
	a.mu.Lock()
	a.titleGen = s
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

// SetPreRun 设置每轮生成开始前的回调（如 MCP 连接与工具注册，要求幂等）。
// 回调在每次 runOnce 开头调用，失败中止本轮生成；nil 清除回调。
func (a *Agent) SetPreRun(fn func(ctx context.Context) error) {
	a.preRun = fn
}

// Notify 广播一条系统提示（TUI 展示为 system 消息；ACP 翻译器忽略）。
func (a *Agent) Notify(text string) {
	a.emit(SystemNoticeEvent{Text: text})
}

// History 返回当前会话的模型消息历史（/sessions 切换会话后 UI 渲染用）。
func (a *Agent) History() []llm.Message {
	return a.sess.ToMessages()
}

// Turns 返回当前会话的对话轮次（user 消息数；压缩摘要存于 meta，不计入）。
func (a *Agent) Turns() int {
	return countTurns(a.sess.ToMessages())
}

// MaxTurns 返回单会话对话轮次上限（与 Run/Compress 的拒绝阈值一致）。
func (a *Agent) MaxTurns() int {
	return maxConversationTurns
}

// Run 执行一轮对话：追加用户消息 → 模型生成（含工具循环）→ 落盘。
func (a *Agent) Run(ctx context.Context, userMsg string) error {
	return a.run(ctx, userMsg, nil)
}

// RunWithImages 执行一轮对话并附加外部提供的图片（ACP 等协议通道的
// image content block）。
//
// 与 Run 的差异：图片以内存 data URI 形式进入模型上下文，**不写入会话文件**
// （当轮有效，会话恢复后不可重放，需用户重新引用）；而 Run 的文本 @ 引用
// 走"存绝对路径 → 恢复时重读"的持久化语义（见 parseImageMentions）。
func (a *Agent) RunWithImages(ctx context.Context, userMsg string, images []llm.Image) error {
	return a.run(ctx, userMsg, images)
}

// run 是 Run / RunWithImages 的共同实现。
// extImages 为外部注入的当轮图片（无本地文件，nil 表示纯文本消息）。
func (a *Agent) run(ctx context.Context, userMsg string, extImages []llm.Image) error {
	// 空文本仅在携带外部图片时放行（ACP 客户端可能发纯图 prompt）：
	// 纯文本空消息仍拒绝。
	if strings.TrimSpace(userMsg) == "" && len(extImages) == 0 {
		return errors.New("消息不能为空")
	}
	// 轮次上限：达到 maxConversationTurns 轮后拒绝继续对话。
	// 检查在 AppendUser 之前，超限的用户消息不写入会话。
	if countTurns(a.sess.ToMessages()) >= maxConversationTurns {
		return fmt.Errorf("Conversation limit reached: this session has exceeded %d turns. Response quality degrades on very long sessions. Start a new session with /new.", maxConversationTurns)
	}
	// @ 图片引用解析：命中图片则剥离 token 并注入图片；非图片引用
	// （文本文件/目录/不存在等）一律不消费、原样保留，交由模型自行判断
	// （如调用 read_file 读取）。图片引用失败（读取失败/超限）时中止本轮，
	// 不写会话——用户明确指向的图片必须可读，静默会误导模型。
	text, imgs, err := parseImageMentions(a.cwd, userMsg)
	if err != nil {
		return err
	}
	wasFirst := !a.sess.HasTitle()
	if err := a.sess.AppendUser(text, imgs...); err != nil {
		return fmt.Errorf("保存用户消息失败: %w", err)
	}
	// 首条用户消息：异步生成 AI 标题（全局默认模型），成功则覆写截断标题并广播
	if wasFirst {
		// 快照会话指针与 ID，避免并发切换会话时写错文件
		sessSnap := a.sess
		sessID := sessSnap.ID
		// 文本为空且仅有图片时不生成标题（无有效文本）
		if strings.TrimSpace(text) != "" {
			a.spawnTitleGen(text, sessID, sessSnap)
		}
	}
	for _, img := range imgs {
		a.emit(SystemNoticeEvent{Text: "Attached image: " + img.Path})
	}
	// 外部注入图片（无本地文件）：当轮暂存，buildHistory 组装请求时合并
	// 到最后一条 user 消息；runOnce 返回后清空（本轮生命周期结束）。
	a.pendingImages = extImages
	defer func() { a.pendingImages = nil }()
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

// Compress 压缩当前会话上下文（/compress）：用当前模型对全量历史做一次
// 无工具总结，流式输出总结内容（TextDeltaEvent 广播，UI/ACP 直接展示），
// 成功后将摘要写入会话 meta（context_summary），后续请求经 runOnce 作为
// 头部上下文注入，缓解截断造成的上下文不连续。
//
// 约束（与 Run 的对话上限一致，不豁免）：
//   - 达到 maxConversationTurns 轮后禁止压缩（提示新开会话）
//   - 每会话最多压缩 1 次（SummarySet 置位后拒绝）
//
// 总结输入为全量历史（未截断）：摘要须覆盖全部早期内容才有价值。
func (a *Agent) Compress(ctx context.Context) error {
	if countTurns(a.sess.ToMessages()) >= maxConversationTurns {
		return errors.New("Conversation limit reached: this session has exceeded 60 turns. Start a new session with /new.")
	}
	if _, ok := a.sess.GetSummary(); ok {
		return errors.New("Conversation already compressed: each session allows at most one compression.")
	}
	if countTurns(a.sess.ToMessages()) < compressMinTurns {
		return fmt.Errorf("Cannot compress yet: this session has fewer than %d turns. Compression is only useful for longer conversations.", compressMinTurns)
	}

	// 锁内取当前模型流：/switch 可并发替换（与 runOnce 一致）
	a.mu.Lock()
	streamer := a.streamer
	a.mu.Unlock()

	stream, err := streamer.StreamText(ctx, llm.StreamRequest{
		System:   compressSystemPrompt,
		Messages: a.sess.ToMessages(), // 全量历史：总结输入需完整
		MaxSteps: a.cfg.Agent.MaxSteps,
	})
	if err != nil {
		return fmt.Errorf("模型调用失败: %w", err)
	}

	// 消费流（无工具，仅文本增量 + Finish）
	var fullText strings.Builder
	var usage llm.Usage
	for ch := range stream.Chunks() {
		switch {
		case ch.Err != nil:
			return ch.Err
		case ch.Text != "":
			fullText.WriteString(ch.Text)
			a.emit(TextDeltaEvent{Text: ch.Text})
		case ch.Finish:
			usage = ch.Usage
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}

	a.emit(TextDoneEvent{FullText: fullText.String()})
	// 空总结视为压缩失败：不写 meta、不消耗唯一 1 次压缩机会（可重试）
	if strings.TrimSpace(fullText.String()) == "" {
		return errors.New("Compression failed: the model returned an empty summary. Please try again.")
	}
	if err := a.sess.AppendSummary(fullText.String()); err != nil {
		return fmt.Errorf("保存压缩摘要失败: %w", err)
	}
	a.emit(DoneEvent{Usage: usage})
	return nil
}

// buildPrompt 组装当前模式的系统提示词（含项目 AGENTS.md 与 skills 列表，
// 每次实时读取：/init、手改 AGENTS.md 或 skills 变更后下一次对话立即生效，无需重启）。
func (a *Agent) buildPrompt() string {
	toolList := a.registry.ForMode(tools.Mode(a.mode))
	projectCtx := ""
	if a.cfg.Agent.LoadAgentsMD {
		projectCtx = loadProjectContext(a.cwd)
	}
	return buildSystemPrompt(a.cwd, a.mode, toolDescriptions(toolList), projectCtx, a.skillDescriptions())
}

// skillDescriptions 提取 skills 描述列表（"name: description (source: ...)"，
// 注入系统提示词；skills 未启用时返回 nil）。
func (a *Agent) skillDescriptions() []string {
	if a.skills == nil {
		return nil
	}
	infos := a.skills.List()
	out := make([]string, 0, len(infos))
	for _, s := range infos {
		out = append(out, fmt.Sprintf("%s: %s (source: %s)", s.Name, s.Description, s.Source))
	}
	return out
}

// runOnce 是核心生成循环（Run/RunInit 共用）：截断 → 组装请求 → StreamText
// → 事件 → 落盘。用户消息须已由调用方 AppendUser。
func (a *Agent) runOnce(ctx context.Context, system string) error {
	// 每轮前置回调（如 MCP 连接与工具注册，调用方保证幂等）：失败中止本轮
	if a.preRun != nil {
		if err := a.preRun(ctx); err != nil {
			return err
		}
	}
	// 上下文截断 + 压缩摘要头部注入（截断按轮次：超过
	// defaultMaxHistoryTurns 轮才丢弃最早整轮）
	history := a.buildHistory(true)

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

	// 消费流（工具调用/结果/文本/思考实时广播与落盘）
	fullText, fullReasoning, usage, stepsExhausted, err := a.consumeStream(stream)
	if err != nil {
		return err
	}

	// 工具循环步数耗尽（模型仍想继续调用工具但已达 MaxSteps 上限）：
	// goai 按正常结束返回，若不处理用户会看到"无输出直接停止"。
	// 自动发起一轮无工具收尾生成，让模型基于已有工具结果输出总结。
	if stepsExhausted {
		a.emit(SystemNoticeEvent{Text: fmt.Sprintf("Tool-call limit (%d) reached while the model still needed more steps. Requesting a summary of progress.", a.cfg.Agent.MaxSteps)})
		fText, fReasoning, fUsage, fErr := a.finalizeTurn(ctx)
		// 收尾请求的 token 消耗无论成败都计入用量（状态栏展示真实消耗）
		usage.InputTokens += fUsage.InputTokens
		usage.OutputTokens += fUsage.OutputTokens
		usage.TotalTokens += fUsage.TotalTokens
		if fErr != nil || strings.TrimSpace(fText) == "" {
			// 用户主动取消（Esc）：与主流程一致以 ctx 错误结束，
			// 不落占位文本（占位说明会让用户误以为异常中断）。
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 其余失败/为空：落盘占位说明（会话历史完整可恢复），
			// 不向上抛错——主循环的工具执行已完成，报错无助于用户。
			// 主循环与收尾已流式输出的文本、思考内容均保留（拼接而非
			// 替换），保证 UI 已展示的内容与落盘历史一致。
			if fErr != nil {
				a.emit(SystemNoticeEvent{Text: "The follow-up summary also failed: " + fErr.Error()})
			}
			fullText = joinTurnText(joinTurnText(fullText, fText), exhaustedPlaceholder(a.cfg.Agent.MaxSteps))
			if fReasoning != "" {
				if fullReasoning != "" {
					fullReasoning += "\n"
				}
				fullReasoning += fReasoning
			}
		} else {
			// 收尾总结追加到主循环已输出的部分文本之后（主循环最后一步
			// 通常无文本，但更早步骤可能已输出过程性内容，不应丢弃，
			// 否则 UI 已展示的内容与落盘历史不一致）；思考内容同理保留。
			fullText = joinTurnText(fullText, fText)
			if fReasoning != "" {
				if fullReasoning != "" {
					fullReasoning += "\n"
				}
				fullReasoning += fReasoning
			}
		}
	}

	a.emit(TextDoneEvent{FullText: fullText})
	if err := a.sess.AppendAssistant(fullText, fullReasoning, &usage); err != nil {
		return fmt.Errorf("保存助手回复失败: %w", err)
	}
	a.emit(DoneEvent{Usage: usage})
	return nil
}

// consumeStream 消费生成流并广播事件（runOnce 与 finalizeTurn 共用）：
// 文本/思考增量实时广播，工具调用/结果记录会话；返回完整文本、思考、
// token 用量与是否因 MaxSteps 上限被截断（StepsExhausted）。
func (a *Agent) consumeStream(stream llm.Stream) (text, reasoning string, usage llm.Usage, stepsExhausted bool, err error) {
	var fullText strings.Builder
	var fullReasoning strings.Builder
	thinkingStarted := false // 已广播 ThinkingStartEvent（仅首个 reasoning 增量触发一次）
	for ch := range stream.Chunks() {
		switch {
		case ch.Err != nil:
			// 出错也返回已累积的文本/思考：上层（如收尾失败兜底）可能
			// 需要保留 UI 已流式展示的内容（主循环错误路径不落盘，忽略即可）。
			return fullText.String(), fullReasoning.String(), usage, false, ch.Err
		case ch.Reasoning != "":
			// 思考增量：首个触发 ThinkingStartEvent（UI 切换 [thinking...]），
			// 每个增量广播 ReasoningDeltaEvent（ACP 层转 agent_thought_chunk），
			// 并累积拼接供落盘（reasoning 不进模型上下文，仅展示/回放用）。
			if !thinkingStarted {
				thinkingStarted = true
				a.emit(ThinkingStartEvent{})
			}
			fullReasoning.WriteString(ch.Reasoning)
			a.emit(ReasoningDeltaEvent{Text: ch.Reasoning})
		case ch.Text != "":
			fullText.WriteString(ch.Text)
			a.emit(TextDeltaEvent{Text: ch.Text})
		case ch.ToolCall != nil:
			a.onToolCall(*ch.ToolCall)
		case ch.Finish:
			usage = ch.Usage
			stepsExhausted = ch.StepsExhausted
		}
	}
	if err := stream.Err(); err != nil {
		return fullText.String(), fullReasoning.String(), usage, false, err
	}
	return fullText.String(), fullReasoning.String(), usage, stepsExhausted, nil
}

// finalizeTurn 发起一轮无工具收尾生成（工具步数耗尽后调用）：
// 基于会话中已执行的完整工具结果（实时 ToMessages），让模型输出最终
// 总结/现状说明；文本经 consumeStream 流式广播，作为本次回复落盘。
func (a *Agent) finalizeTurn(ctx context.Context) (text, reasoning string, usage llm.Usage, err error) {
	// 锁内取当前模型流：/switch 可并发替换（与 runOnce 一致）
	a.mu.Lock()
	streamer := a.streamer
	a.mu.Unlock()

	stream, err := streamer.StreamText(ctx, llm.StreamRequest{
		System: finalizeSystemPrompt,
		// 与主循环同一视图：按轮次截断 + 压缩摘要头部注入，
		// 长会话下避免收尾请求上下文爆炸或与主循环历史不一致。
		// noteTruncation=false：截断记录由主循环写过，不重复写 meta。
		Messages:        a.buildHistory(false),
		MaxSteps:        1, // 无工具注册，单步文本生成
		ReasoningEffort: a.thinking,
	})
	if err != nil {
		return "", "", usage, fmt.Errorf("模型调用失败: %w", err)
	}
	text, reasoning, usage, _, err = a.consumeStream(stream)
	return text, reasoning, usage, err
}

// onToolCall 处理模型请求的工具调用：记录会话 + 广播事件。
func (a *Agent) onToolCall(tc llm.ToolCall) {
	a.sess.AppendToolCall(tc.ID, tc.Name, tc.Input)
	a.emit(ToolCallEvent{CallID: tc.ID, Name: tc.Name, Input: tc.Input})
}

// onBeforeToolExecute 是工具执行前的权限确认拦截点。
// 拒绝时工具不执行，拒绝原因作为工具结果返回给模型（模型可调整方案）。
//
// 注意：goai 并行执行工具调用时会并发进入本函数；确认器只支持单个挂起
// 请求，必须串行化（approvalMu）——否则并发确认互相覆盖导致永久阻塞。
func (a *Agent) onBeforeToolExecute(info llm.BeforeToolExecuteInfo) llm.BeforeToolExecuteResult {
	a.approvalMu.Lock()
	defer a.approvalMu.Unlock()

	need, summary := a.registry.NeedApproveFor(info.Name, info.Input)
	if !need {
		return llm.BeforeToolExecuteResult{}
	}

	decision, err := a.approver.Request(info.Ctx, ApprovalRequest{
		CallID:  info.CallID,
		Tool:    info.Name,
		Summary: summary,
		Input:   info.Input,
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

// titleSystemPrompt 是标题生成的系统提示词（单轮无工具、低 token、单句）。
const titleSystemPrompt = "Summarize into a title, 10-25 Chinese characters or 10-18 English words. Output title only, single line."

// spawnTitleGen 异步生成标题：使用全局默认模型（config.DefaultModelName），
// 成功则覆写会话标题并广播 TitleUpdatedEvent；空/失败保留原截断标题。
// 调用方已确认 wasFirst 且 text 非空；sessSnap 为触发时的会话指针快照。
func (a *Agent) spawnTitleGen(content, sessID string, sessSnap *session.Session) {
	trimmed := strings.TrimSpace(content)
	if len([]rune(trimmed)) < 4 {
		return
	}
	runes := []rune(trimmed)
	if len(runes) > 2000 {
		trimmed = string(runes[:2000])
	}
	go func() {
		var streamer llm.Streamer
		a.mu.Lock()
		if a.titleGen != nil {
			streamer = a.titleGen
		}
		a.mu.Unlock()
		if streamer == nil {
			spec, err := a.cfg.DefaultModelName()
			if err != nil {
				return
			}
			m, err := llm.BuildModelSpec(a.cfg, spec)
			if err != nil {
				return
			}
			streamer = llm.Bind(m)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// 标题生成对 reasoning 要求苛刻：muse-spark 等 openai.responses 模型不支持 reasoning_effort=none（400），
		// 空 effort 又会触发 7-8k 思考导致 "http2: response body closed"；pro 模型用 none+1k 限流最快(2.8s)但部分模型需回退到 low。
		// 策略：先试 none（最省 token、最快），若返回 invalid_request / body closed 则回退到 low 重试一次。
		tryTitle := func(effort string) (string, error) {
			stream, err := streamer.StreamText(ctx, llm.StreamRequest{
				System:          titleSystemPrompt,
				Messages:        []llm.Message{{Role: llm.RoleUser, Content: trimmed}},
				MaxSteps:        1,
				ReasoningEffort: effort,
			})
			if err != nil {
				return "", err
			}
			var sb strings.Builder
			for ch := range stream.Chunks() {
				if ch.Err != nil {
					return "", ch.Err
				}
				if ch.Text != "" {
					sb.WriteString(ch.Text)
				}
			}
			if err := stream.Err(); err != nil {
				return "", err
			}
			return strings.TrimSpace(sb.String()), nil
		}
		raw, err := tryTitle("none")
		if err != nil && isTitleRetryable(err) {
			a.emit(SystemNoticeEvent{Text: "[title] retry with low after: " + err.Error()})
			raw, err = tryTitle("low")
		}
		if err != nil {
			a.emit(SystemNoticeEvent{Text: "[title] StreamText failed: " + err.Error()})
			return
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			a.emit(SystemNoticeEvent{Text: "[title] empty response"})
			return
		}
		if idx := strings.Index(raw, "\n"); idx >= 0 {
			raw = strings.TrimSpace(raw[:idx])
		}
		if raw == "" {
			a.emit(SystemNoticeEvent{Text: "[title] empty after first line"})
			return
		}
		if sessSnap.ID != sessID {
			return
		}
		before := sessSnap.GetTitle()
		if err := sessSnap.UpdateTitle(raw); err != nil {
			a.emit(SystemNoticeEvent{Text: "[title] UpdateTitle failed: " + err.Error()})
			return
		}
		after := sessSnap.GetTitle()
		if before == after {
			// 标题相同（模型复述了截断标题），仍视为成功但不额外通知
			return
		}
		a.emit(TitleUpdatedEvent{Title: after, SessionID: sessID})
	}()
}

// isTitleRetryable 判断标题生成错误是否值得用 low 重试：
// - openai.responses 对 none 返回 invalid_request: reasoning.effort does not support none
// - 空 effort 触发 pro 模型 7-8k 思考导致 http2: response body closed
func isTitleRetryable(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "reasoning") ||
		strings.Contains(s, "effort") ||
		strings.Contains(s, "does not support") ||
		strings.Contains(s, "response body closed") ||
		strings.Contains(s, "invalid_request")
}
