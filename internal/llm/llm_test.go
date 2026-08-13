package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/helloxz/zlite/internal/config"
	"github.com/zendev-sh/goai"
)

// ---- 测试工具：fake OpenAI 兼容端点 ----

func newFakeServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sse(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func textChunk(content string) string {
	b, _ := json.Marshal(content)
	return fmt.Sprintf(`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, b)
}

func finishChunk(in, out int) string {
	return fmt.Sprintf(`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`, in, out, in+out)
}

func toolFinishChunk() string {
	return `{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
}

func toolCallChunk(id, name, args string) string {
	return fmt.Sprintf(`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`, id, name, args)
}

func readBody(t *testing.T, body *strings.Builder) string {
	t.Helper()
	return body.String()
}

func drainChunks(t *testing.T, s Stream) (text string, usage Usage, toolCalls []ToolCall, errs []error) {
	t.Helper()
	for ch := range s.Chunks() {
		if ch.Err != nil {
			errs = append(errs, ch.Err)
			continue
		}
		text += ch.Text
		if ch.ToolCall != nil {
			toolCalls = append(toolCalls, *ch.ToolCall)
		}
		if ch.Finish {
			usage = ch.Usage
		}
	}
	return text, usage, toolCalls, errs
}

// ---- 单元测试 ----

func TestBuildModel(t *testing.T) {
	p := &config.Provider{
		Name:    "default",
		Type:    config.TypeOpenAICompatible,
		BaseURL: "http://127.0.0.1:9/v1",
		APIKey:  "sk-test",
		Model:   "test-model",
	}
	m, err := BuildModel(p)
	if err != nil {
		t.Fatalf("BuildModel 失败: %v", err)
	}
	if m == nil || m.ModelID() != "test-model" {
		t.Fatalf("BuildModel 返回异常: %v", m)
	}

	// 空 APIKey 也应可构造（本地无鉴权端点）
	p.APIKey = ""
	if _, err := BuildModel(p); err != nil {
		t.Fatalf("空 APIKey 构造失败: %v", err)
	}
}

func TestToProviderMessages(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "你好"},
		{Role: RoleAssistant, Content: "文本", ToolCalls: []ToolCall{
			{ID: "call_1", Name: "read_file", Input: map[string]any{"path": "a.go"}},
		}},
		{Role: RoleTool, Content: "文件内容", ToolCallID: "call_1", ToolName: "read_file"},
	}
	out := ToProviderMessages(msgs)
	if len(out) != 3 {
		t.Fatalf("期望 3 条消息，得到 %d", len(out))
	}
	if string(out[0].Role) != "user" || len(out[0].Content) != 1 {
		t.Errorf("user 消息转换错误: %+v", out[0])
	}
	if len(out[1].Content) != 2 {
		t.Fatalf("assistant 消息应有 text + tool-call 两个 part，得到 %d", len(out[1].Content))
	}
	if out[1].Content[1].Type != "tool-call" || out[1].Content[1].ToolName != "read_file" {
		t.Errorf("tool-call part 转换错误: %+v", out[1].Content[1])
	}
	if string(out[2].Role) != "tool" || out[2].Content[0].ToolCallID != "call_1" {
		t.Errorf("tool 消息转换错误: %+v", out[2])
	}
}

// ---- 流式文本 + usage + 认证头 ----

func TestStreamTextBasic(t *testing.T) {
	var lastReqBody atomic.Value
	srv := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastReqBody.Store(string(body))
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization 头错误: %q", got)
		}
		sse(w, textChunk("你好"), textChunk("，世界"), finishChunk(10, 2))
	})

	p := &config.Provider{BaseURL: srv.URL + "/v1", APIKey: "sk-test", Model: "m"}
	model, err := BuildModel(p)
	if err != nil {
		t.Fatal(err)
	}

	stream, err := StreamText(context.Background(), model, StreamRequest{
		System:   "你是助手",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		MaxSteps: 1,
	})
	if err != nil {
		t.Fatalf("StreamText 失败: %v", err)
	}

	text, usage, _, errs := drainChunks(t, stream)
	if len(errs) > 0 {
		t.Fatalf("流错误: %v", errs)
	}
	if text != "你好，世界" {
		t.Errorf("流式文本错误: %q", text)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 2 {
		t.Errorf("usage 错误: %+v", usage)
	}
	if err := stream.Err(); err != nil {
		t.Errorf("stream.Err() = %v", err)
	}

	reqBody := lastReqBody.Load().(string)
	if !strings.Contains(reqBody, `"你是助手"`) || !strings.Contains(reqBody, `"hi"`) {
		t.Errorf("请求体缺少 system/user 消息: %s", reqBody)
	}
}

// ---- 工具循环：模型先请求工具，执行后继续生成 ----

func TestStreamTextToolLoop(t *testing.T) {
	var reqCount atomic.Int32
	var secondReqBody atomic.Value

	srv := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := reqCount.Add(1)
		if n == 1 {
			// 第一次：模型请求工具
			sse(w, toolCallChunk("call_1", "get_weather", `{"city":"Beijing"}`), toolFinishChunk())
			return
		}
		// 第二次：应包含工具结果，然后正常回复
		secondReqBody.Store(string(body))
		sse(w, textChunk("晴天 25°C"), finishChunk(20, 5))
	})

	p := &config.Provider{BaseURL: srv.URL + "/v1", Model: "m"}
	model, err := BuildModel(p)
	if err != nil {
		t.Fatal(err)
	}

	executed := false
	tool := goai.NewTool("get_weather", "查询天气",
		func(ctx context.Context, in struct {
			City string `json:"city"`
		}) (string, error) {
			executed = true
			return "晴天 25°C", nil
		})

	var beforeCalls, startCalls, resultCalls atomic.Int32
	var beforeResult atomic.Value
	stream, err := StreamText(context.Background(), model, StreamRequest{
		System:   "sys",
		Messages: []Message{{Role: RoleUser, Content: "北京天气?"}},
		Tools:    []goai.Tool{tool},
		MaxSteps: 4,
		Hooks: Hooks{
			BeforeToolExecute: func(info BeforeToolExecuteInfo) BeforeToolExecuteResult {
				beforeCalls.Add(1)
				beforeResult.Store(info.Name + ":" + info.Input["city"].(string))
				return BeforeToolExecuteResult{}
			},
			ToolStart: func(info ToolStartInfo) { startCalls.Add(1) },
			ToolResult: func(info ToolResultInfo) {
				resultCalls.Add(1)
				if info.Error != nil {
					t.Errorf("工具执行不应失败: %v", info.Error)
				}
			},
		},
	})
	if err != nil {
		t.Fatalf("StreamText 失败: %v", err)
	}

	text, _, toolCalls, errs := drainChunks(t, stream)
	if len(errs) > 0 {
		t.Fatalf("流错误: %v", errs)
	}
	if !executed {
		t.Error("工具未被执行")
	}
	if text != "晴天 25°C" {
		t.Errorf("最终文本错误: %q", text)
	}
	if len(toolCalls) != 1 || toolCalls[0].Name != "get_weather" || toolCalls[0].ID != "call_1" {
		t.Errorf("工具调用 chunk 错误: %+v", toolCalls)
	}
	if got := beforeCalls.Load(); got != 1 {
		t.Errorf("BeforeToolExecute 调用次数 = %d", got)
	}
	if got := startCalls.Load(); got != 1 {
		t.Errorf("ToolStart 调用次数 = %d", got)
	}
	if got := resultCalls.Load(); got != 1 {
		t.Errorf("ToolResult 调用次数 = %d", got)
	}
	if got := beforeResult.Load().(string); got != "get_weather:Beijing" {
		t.Errorf("hook 参数错误: %q", got)
	}

	// 第二次请求应包含 tool 消息
	body := secondReqBody.Load().(string)
	if !strings.Contains(body, `"tool"`) || !strings.Contains(body, `"call_1"`) {
		t.Errorf("第二次请求缺少工具结果消息: %s", body)
	}
}

// ---- 权限拒绝路径：BeforeToolExecute 返回 Skip，工具不执行 ----

func TestStreamTextToolLoopDenied(t *testing.T) {
	var reqCount atomic.Int32
	var secondReqBody atomic.Value

	srv := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if reqCount.Add(1) == 1 {
			sse(w, toolCallChunk("call_1", "get_weather", `{"city":"Beijing"}`), toolFinishChunk())
			return
		}
		secondReqBody.Store(string(body))
		sse(w, textChunk("抱歉，无法执行"), finishChunk(15, 4))
	})

	p := &config.Provider{BaseURL: srv.URL + "/v1", Model: "m"}
	model, err := BuildModel(p)
	if err != nil {
		t.Fatal(err)
	}

	executed := false
	tool := goai.NewTool("get_weather", "查询天气",
		func(ctx context.Context, in struct {
			City string `json:"city"`
		}) (string, error) {
			executed = true
			return "晴天", nil
		})

	stream, err := StreamText(context.Background(), model, StreamRequest{
		System:   "sys",
		Messages: []Message{{Role: RoleUser, Content: "北京天气?"}},
		Tools:    []goai.Tool{tool},
		MaxSteps: 4,
		Hooks: Hooks{
			BeforeToolExecute: func(info BeforeToolExecuteInfo) BeforeToolExecuteResult {
				return BeforeToolExecuteResult{Skip: true, Result: "用户拒绝了该操作"}
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	text, _, _, errs := drainChunks(t, stream)
	if len(errs) > 0 {
		t.Fatalf("流错误: %v", errs)
	}
	if executed {
		t.Error("被拒绝的工具不应执行")
	}
	if text != "抱歉，无法执行" {
		t.Errorf("最终文本错误: %q", text)
	}
	body := secondReqBody.Load().(string)
	if !strings.Contains(body, "用户拒绝了该操作") {
		t.Errorf("拒绝原因未传给模型: %s", body)
	}
}
