package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/llm"
	"github.com/helloxz/zlite/internal/session"
	"github.com/helloxz/zlite/internal/tools"
	"github.com/zendev-sh/goai"
)

// ---- fake streamer ----

type fakeStream struct {
	chunks []llm.Chunk
	err    error
	ch     chan llm.Chunk
}

func newFakeStream(chunks []llm.Chunk, err error) *fakeStream {
	f := &fakeStream{chunks: chunks, err: err}
	f.ch = make(chan llm.Chunk, len(chunks))
	for _, c := range chunks {
		f.ch <- c
	}
	close(f.ch)
	return f
}

func (f *fakeStream) Chunks() <-chan llm.Chunk { return f.ch }
func (f *fakeStream) Err() error               { return f.err }

type fakeStreamer struct {
	reqs      []llm.StreamRequest
	chunks    []llm.Chunk
	streamErr error // Err() 返回的流错误
	err       error // StreamText 返回的错误
}

func (f *fakeStreamer) StreamText(ctx context.Context, req llm.StreamRequest) (llm.Stream, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return newFakeStream(f.chunks, f.streamErr), nil
}

// ---- 测试辅助 ----

const testCwd = "/data/apps/zlite"

func newTestSession(t *testing.T) *session.Session {
	t.Helper()
	m := session.NewManager(t.TempDir())
	s, err := m.Create(testCwd, &config.Provider{Name: "default", Model: "test-model"}, config.ModePlan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestAgent(t *testing.T, fs *fakeStreamer) *Agent {
	t.Helper()
	cfg := config.DefaultConfig()
	reg := tools.New(testCwd, nil)
	sess := newTestSession(t)
	return New(cfg, fs, reg, sess, NewApprover(false), testCwd, ModePlan)
}

// runAndCollect 同步运行一轮并收集全部事件。
func runAndCollect(t *testing.T, a *Agent, msg string) []Event {
	t.Helper()
	ctx := context.Background()
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx, msg) }()

	var evs []Event
	runDone := false
	for {
		select {
		case ev := <-a.Events():
			evs = append(evs, ev)
			if _, ok := ev.(DoneEvent); ok {
				if !runDone {
					if err := <-errCh; err != nil {
						t.Fatalf("Run 失败: %v", err)
					}
				}
				return evs
			}
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Run 失败: %v", err)
			}
			runDone = true
			// Run 成功，继续消费直到 DoneEvent
		}
	}
}

func hasEvent(evs []Event, want Event) bool {
	for _, ev := range evs {
		switch w := want.(type) {
		case ToolCallEvent:
			if e, ok := ev.(ToolCallEvent); ok && e.Name == w.Name && e.CallID == w.CallID {
				return true
			}
		case ModeChangeEvent:
			if e, ok := ev.(ModeChangeEvent); ok && e.Mode == w.Mode {
				return true
			}
		default:
			if _, ok := ev.(DoneEvent); ok && w == nil {
				return true
			}
		}
	}
	return false
}

// ---- 测试 ----

func TestRunBasicText(t *testing.T) {
	fs := &fakeStreamer{chunks: []llm.Chunk{
		{Text: "你好"},
		{Text: "，世界"},
		{Finish: true, Usage: llm.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}},
	}}
	a := newTestAgent(t, fs)
	evs := runAndCollect(t, a, "打个招呼")

	// 事件序列
	if len(evs) < 4 {
		t.Fatalf("事件过少: %+v", evs)
	}
	if _, ok := evs[0].(TextDeltaEvent); !ok {
		t.Errorf("首个事件应为 TextDelta: %+v", evs[0])
	}
	last := evs[len(evs)-1]
	done, ok := last.(DoneEvent)
	if !ok || done.Usage.InputTokens != 5 || done.Usage.OutputTokens != 3 {
		t.Errorf("末尾事件应为 DoneEvent(usage): %+v", last)
	}

	// 请求参数
	req := fs.reqs[0]
	if req.MaxSteps != config.DefaultConfig().Agent.MaxSteps {
		t.Errorf("MaxSteps 错误: %d", req.MaxSteps)
	}
	if len(req.Tools) != 5 {
		t.Errorf("plan 模式应注入 5 个工具，得到 %d", len(req.Tools))
	}
	if !strings.Contains(req.System, "plan") || !strings.Contains(req.System, "read_file") {
		t.Errorf("system prompt 应含模式与工具: %q", req.System)
	}

	// 会话落盘
	msgs := a.sess.ToMessages()
	if len(msgs) != 2 {
		t.Fatalf("会话应有 user+assistant 两条，得到 %d", len(msgs))
	}
	if msgs[0].Content != "打个招呼" || msgs[1].Content != "你好，世界" {
		t.Errorf("会话内容错误: %+v", msgs)
	}
}

func TestRunToolCallRecord(t *testing.T) {
	fs := &fakeStreamer{chunks: []llm.Chunk{
		{Text: "先查一下"},
		{ToolCall: &llm.ToolCall{ID: "call_1", Name: "read_file", Input: map[string]any{"path": "a.go"}}},
		{Finish: true},
	}}
	a := newTestAgent(t, fs)
	evs := runAndCollect(t, a, "读文件")

	if !hasEvent(evs, ToolCallEvent{CallID: "call_1", Name: "read_file"}) {
		t.Errorf("应有 ToolCallEvent: %+v", evs)
	}
	msgs := a.sess.ToMessages()
	if len(msgs) != 3 {
		t.Fatalf("会话应有 user+assistant+tool_call，得到 %d: %+v", len(msgs), msgs)
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].ID != "call_1" {
		t.Errorf("tool_call 未正确记录: %+v", msgs[1])
	}
}

func TestRunModeFiltering(t *testing.T) {
	fs := &fakeStreamer{chunks: []llm.Chunk{{Finish: true}}}
	a := newTestAgent(t, fs)

	// 注册一个仅 build 可见的工具
	a.registry.Register(tools.Tool{
		GoAITool: goai.NewTool("fake_write", "测试写工具", func(ctx context.Context, in struct{}) (string, error) { return "ok", nil }),
		Modes:    []tools.Mode{tools.ModeBuild},
	})

	runAndCollect(t, a, "plan 下")
	if n := len(fs.reqs[0].Tools); n != 5 {
		t.Errorf("plan 模式应 5 个工具（4 只读 + run_command 只读版，无 fake_write），得到 %d", n)
	}

	a.SetMode(ModeBuild)
	runAndCollect(t, a, "build 下")
	if n := len(fs.reqs[1].Tools); n != 9 {
		t.Errorf("build 模式应 9 个工具（4 只读 + run_command 全量版 + write/edit/delete + fake_write），得到 %d", n)
	}
}

func TestRunContextTruncation(t *testing.T) {
	fs := &fakeStreamer{chunks: []llm.Chunk{{Finish: true}}}
	a := newTestAgent(t, fs)

	// 预置 45 条历史
	for i := 0; i < 45; i++ {
		a.sess.AppendUser("历史消息")
	}

	runAndCollect(t, a, "新消息")
	msgs := fs.reqs[0].Messages
	if len(msgs) != defaultMaxHistoryMessages {
		t.Errorf("截断后应为 %d 条，得到 %d", defaultMaxHistoryMessages, len(msgs))
	}
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser || last.Content != "新消息" {
		t.Errorf("最新消息必须保留: %+v", last)
	}
}

func TestRunEmptyMessage(t *testing.T) {
	fs := &fakeStreamer{}
	a := newTestAgent(t, fs)
	if err := a.Run(context.Background(), "   "); err == nil {
		t.Fatal("空消息应报错")
	}
}

func TestRunStreamerError(t *testing.T) {
	// StreamText 直接失败
	fs := &fakeStreamer{err: context.DeadlineExceeded}
	a := newTestAgent(t, fs)
	if err := a.Run(context.Background(), "hi"); err == nil {
		t.Fatal("streamer 错误应传播")
	}

	// chunk 流内错误
	fs2 := &fakeStreamer{chunks: []llm.Chunk{{Err: context.Canceled}}}
	a2 := newTestAgent(t, fs2)
	if err := a2.Run(context.Background(), "hi"); err == nil {
		t.Fatal("chunk 错误应传播")
	}
}

func TestSetMode(t *testing.T) {
	fs := &fakeStreamer{chunks: []llm.Chunk{{Finish: true}}}
	a := newTestAgent(t, fs)

	if a.Mode() != ModePlan {
		t.Fatalf("初始模式应为 plan: %s", a.Mode())
	}

	evs := runAndCollect(t, a, "先跑一轮")
	if hasEvent(evs, ModeChangeEvent{Mode: ModeBuild}) {
		t.Error("未切换模式不应有 ModeChangeEvent")
	}

	a.SetMode(ModeBuild)
	if a.Mode() != ModeBuild {
		t.Errorf("SetMode 后模式错误: %s", a.Mode())
	}
	evs2 := runAndCollect(t, a, "再跑一轮")
	if !hasEvent(evs2, ModeChangeEvent{Mode: ModeBuild}) {
		t.Error("切换模式应有 ModeChangeEvent")
	}
}

func TestOnBeforeToolExecute(t *testing.T) {
	reg := tools.New(testCwd, nil)
	reg.Register(tools.Tool{
		GoAITool: goai.NewTool("needs_approve", "需确认的工具",
			func(ctx context.Context, in struct{}) (string, error) { return "ok", nil }),
		Modes: []tools.Mode{tools.ModeBuild},
		NeedApprove: func(input map[string]any) (bool, string) {
			return true, "测试摘要"
		},
	})

	cfg := config.DefaultConfig()
	sess := newTestSession(t)

	// nilApprover（默认拒绝）
	a := New(cfg, &fakeStreamer{}, reg, sess, NewApprover(false), testCwd, ModePlan)
	res := a.onBeforeToolExecute(llm.BeforeToolExecuteInfo{
		CallID: "c1", Name: "needs_approve", Input: map[string]any{},
	})
	if !res.Skip || !strings.Contains(res.Result, "用户拒绝") {
		t.Errorf("默认 approver 应拒绝: %+v", res)
	}

	// 无需确认的工具直接放行
	res = a.onBeforeToolExecute(llm.BeforeToolExecuteInfo{CallID: "c2", Name: "read_file"})
	if res.Skip {
		t.Errorf("无需确认的工具不应被拦截: %+v", res)
	}

	// autoApprover（自动批准）
	a2 := New(cfg, &fakeStreamer{}, reg, sess, NewApprover(true), testCwd, ModePlan)
	res = a2.onBeforeToolExecute(llm.BeforeToolExecuteInfo{
		CallID: "c3", Name: "needs_approve", Input: map[string]any{},
	})
	if res.Skip {
		t.Errorf("auto_approve 应放行: %+v", res)
	}
}

func TestTruncateMessagesPairing(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "u1"},
		{Role: llm.RoleAssistant, Content: "a1", ToolCalls: []llm.ToolCall{{ID: "c1"}, {ID: "c2"}}},
		{Role: llm.RoleTool, Content: "r1", ToolCallID: "c1"},
		{Role: llm.RoleTool, Content: "r2", ToolCallID: "c2"},
		{Role: llm.RoleAssistant, Content: "a2"},
	}
	got := truncateMessages(msgs, 3)
	// 从后取 3 条会以 tool 结果开头，配对扩展向前包含 assistant 调用 → 4 条
	if len(got) != 4 {
		t.Fatalf("截断后应为 4 条（配对扩展），得到 %d", len(got))
	}
	// 起始不能是 tool 结果（配对完整性）
	if got[0].Role != llm.RoleAssistant || len(got[0].ToolCalls) != 2 {
		t.Errorf("截断破坏了配对: %+v", got)
	}

	// 不截断的情况
	all := truncateMessages(msgs, 10)
	if len(all) != 5 {
		t.Error("未超限不应截断")
	}
}

func TestParseMode(t *testing.T) {
	if m, err := ParseMode("build"); err != nil || m != ModeBuild {
		t.Errorf("ParseMode(build) 错误: %v %v", m, err)
	}
	if _, err := ParseMode("xxx"); err == nil {
		t.Error("非法模式应报错")
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	p := buildSystemPrompt("/proj", ModePlan, []string{"read_file: 读文件", "grep: 搜索"})
	for _, want := range []string{"zlite", "/proj", "plan", "read_file", "只读"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt 缺少 %q: %s", want, p)
		}
	}
	p2 := buildSystemPrompt("/proj", ModeBuild, nil)
	if !strings.Contains(p2, "可写") {
		t.Errorf("build prompt 应标注可写: %s", p2)
	}
}

func TestSetSession(t *testing.T) {
	fs := &fakeStreamer{chunks: []llm.Chunk{{Finish: true}}}
	a := newTestAgent(t, fs)

	// 旧会话跑一轮
	runAndCollect(t, a, "旧会话消息")
	if n := len(a.sess.ToMessages()); n != 2 {
		t.Fatalf("旧会话应有 2 条消息，得到 %d", n)
	}

	// 切换新会话
	m := session.NewManager(t.TempDir())
	ns, err := m.Create(testCwd, &config.Provider{Name: "default", Model: "m"}, config.ModePlan)
	if err != nil {
		t.Fatal(err)
	}
	a.SetSession(ns)
	if a.sess != ns {
		t.Fatal("SetSession 后 agent 应使用新会话")
	}
	if n := len(a.sess.ToMessages()); n != 0 {
		t.Fatalf("新会话历史应为空，得到 %d", n)
	}

	// 新会话可正常对话与落盘
	runAndCollect(t, a, "新会话消息")
	msgs := a.sess.ToMessages()
	if len(msgs) != 2 || msgs[0].Content != "新会话消息" {
		t.Errorf("新会话对话异常: %+v", msgs)
	}

	// 旧会话文件应可被 Continue 恢复（切换时已 Close，数据完整）
	old, err := m.Continue(testCwd)
	if err != nil {
		t.Fatalf("新会话目录应能 Continue: %v", err)
	}
	old.Close()
	if old.ID != ns.ID {
		t.Errorf("Continue 应找到新会话: %q != %q", old.ID, ns.ID)
	}
}
