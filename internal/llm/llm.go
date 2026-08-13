// Package llm 封装 goai（github.com/zendev-sh/goai）为 zlite 提供模型能力。
//
// 这是唯一允许依赖 goai 的业务包：agent/tools/tui 等包只面向本包定义的
// 轻量类型（Message/Chunk/ToolCall/hooks），后期更换 AI SDK 时只需改本包。
package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/helloxz/zlite/internal/config"
	"github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
	"github.com/zendev-sh/goai/provider/compat"
	"github.com/zendev-sh/goai/provider/openai"
)

// Role 是消息角色。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 是模型请求的工具调用。
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// Message 是一条对话消息（会话历史与模型之间的中间表示）。
type Message struct {
	Role Role
	// Content: user/assistant 的文本；tool 消息的工具输出。
	Content string
	// ToolCalls: assistant 消息附带的工具调用（仅恢复历史时使用，
	// 工具循环过程中由 SDK 内部维护）。
	ToolCalls []ToolCall
	// ToolCallID / ToolName: tool 消息关联的调用。
	ToolCallID string
	ToolName   string
}

// Usage 是 token 用量。
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// Chunk 是流式输出的一次增量。
type Chunk struct {
	// Text 是文本增量（模型回复时逐段产生）。
	Text string
	// ToolCall 是模型请求的完整工具调用（非 nil 表示需要执行工具）。
	ToolCall *ToolCall
	// Finish 表示流结束（此时 Usage 有效）。
	Finish bool
	// Usage 是整轮生成的总 token 用量（Finish 时有效）。
	Usage Usage
	// Err 是流错误（非 nil 时流终止）。
	Err error
}

// ---- hooks（权限确认与工具执行观测，agent 通过 Hooks 注册）----

// BeforeToolExecuteInfo 是工具执行前的拦截信息。
type BeforeToolExecuteInfo struct {
	Ctx    context.Context
	CallID string
	Name   string
	Input  map[string]any
}

// BeforeToolExecuteResult 控制是否执行工具。
type BeforeToolExecuteResult struct {
	// Skip 为 true 时不执行工具，Result 作为工具输出返回给模型。
	Skip   bool
	Result string
	Error  error
}

// ToolStartInfo 是工具开始执行的信息。
type ToolStartInfo struct {
	CallID string
	Name   string
	Input  map[string]any
}

// ToolResultInfo 是工具执行结果的信息。
type ToolResultInfo struct {
	CallID string
	Name   string
	Output string
	Error  error
}

// Hooks 是透传给 goai 的生命周期回调。
type Hooks struct {
	// BeforeToolExecute 在每个工具执行前调用，用于权限确认；
	// 返回 Skip 时工具不执行，Result 会作为工具结果发给模型。
	BeforeToolExecute func(BeforeToolExecuteInfo) BeforeToolExecuteResult
	// ToolStart 在工具开始执行时调用（UI 展示用）。
	ToolStart func(ToolStartInfo)
	// ToolResult 在工具执行完成后调用（UI 展示用）。
	ToolResult func(ToolResultInfo)
}

// StreamRequest 是一次生成请求的参数。
type StreamRequest struct {
	System   string
	Messages []Message
	// Tools 是当前模式可见的工具（由 tools.Registry.ForMode 提供）。
	Tools    []goai.Tool
	MaxSteps int
	Hooks    Hooks
	// ReasoningEffort 是思考强度（none/low/medium/high/xhigh/max）。
	// 空字符串表示不传参（auto），由 API 自行决定；其余值原样透传，
	// 是否支持由后端决定（不支持时 API 报错，错误会经流返回给用户）。
	ReasoningEffort string
}

// Model 包装 goai 的 provider.LanguageModel，并携带 API 格式信息
// （useResponsesAPI 是请求级开关，随每次生成请求注入 ProviderOptions）。
type Model struct {
	lm   provider.LanguageModel
	resp bool // 使用 OpenAI Responses API（/v1/responses）
}

// buildModel 按指定模型名构造模型（BuildModel / BuildModelNamed 共用）：
//   - openai.chat      → compat.Chat（Chat Completions，兼容一切自定义端点）
//   - openai.responses → openai.Chat + useResponsesAPI=true（要求端点支持 /responses）
//
// 未来新增厂商（anthropic/google 等）在此追加分派即可，调用侧不变。
func buildModel(p *config.Provider, model string) (*Model, error) {
	switch p.Type {
	case config.TypeOpenAIChat, "":
		opts := []compat.Option{compat.WithBaseURL(p.BaseURL)}
		if p.APIKey != "" {
			opts = append(opts, compat.WithAPIKey(p.APIKey))
		}
		return &Model{lm: compat.Chat(model, opts...)}, nil
	case config.TypeOpenAIResponses:
		opts := []openai.Option{openai.WithBaseURL(p.BaseURL)}
		if p.APIKey != "" {
			opts = append(opts, openai.WithAPIKey(p.APIKey))
		}
		return &Model{lm: openai.Chat(model, opts...), resp: true}, nil
	default:
		return nil, fmt.Errorf("不支持的 provider type: %q", p.Type)
	}
}

// BuildModel 按 provider 配置的第一个模型构造模型。
func BuildModel(p *config.Provider) (*Model, error) {
	return buildModel(p, p.Models[0])
}

// BuildModelNamed 按指定模型名构造模型（/switch 切换用，其余参数同 BuildModel）。
// 模型名不在此校验，调用方负责从配置的模型列表中选取。
func BuildModelNamed(p *config.Provider, model string) (*Model, error) {
	return buildModel(p, model)
}

// ToProviderMessages 把 zlite 消息转换为 goai provider 消息。
// 历史恢复时 assistant 消息可携带 ToolCalls（转为 tool-call parts）。
func ToProviderMessages(msgs []Message) []provider.Message {
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleUser:
			out = append(out, goai.UserMessage(m.Content))
		case RoleAssistant:
			if len(m.ToolCalls) == 0 {
				out = append(out, goai.AssistantMessage(m.Content))
				continue
			}
			parts := []provider.Part{{Type: provider.PartText, Text: m.Content}}
			for _, tc := range m.ToolCalls {
				raw, err := json.Marshal(tc.Input)
				if err != nil {
					raw = []byte("{}")
				}
				parts = append(parts, provider.Part{
					Type:       provider.PartToolCall,
					ToolCallID: tc.ID,
					ToolName:   tc.Name,
					ToolInput:  raw,
				})
			}
			out = append(out, provider.Message{Role: provider.RoleAssistant, Content: parts})
		case RoleTool:
			out = append(out, goai.ToolMessage(m.ToolCallID, m.ToolName, m.Content))
		}
	}
	return out
}

// Stream 是流式生成的消费视图（包装 goai.TextStream）。
type Stream interface {
	// Chunks 返回转换后的流式增量通道；消费完毕后调用 Err 取结果。
	Chunks() <-chan Chunk
	// Err 返回流错误（消费完 Chunks 后调用）。
	Err() error
}

// Streamer 是模型流式生成能力（agent 依赖此接口，测试可注入 fake）。
type Streamer interface {
	StreamText(ctx context.Context, req StreamRequest) (Stream, error)
}

// modelStreamer 把 Model 绑定为 Streamer。
type modelStreamer struct {
	model *Model
}

func (m modelStreamer) StreamText(ctx context.Context, req StreamRequest) (Stream, error) {
	return StreamText(ctx, m.model, req)
}

// Bind 返回绑定到指定模型的 Streamer。
func Bind(model *Model) Streamer {
	return modelStreamer{model: model}
}

// Stream 包装 goai.TextStream，对外只暴露 zlite 类型。
type goaiStream struct {
	ts *goai.TextStream
}

// StreamText 发起一次流式生成（含多步工具循环，步数由 MaxSteps 控制）。
func StreamText(ctx context.Context, model *Model, req StreamRequest) (Stream, error) {
	opts := []goai.Option{
		goai.WithSystem(req.System),
		goai.WithMessages(ToProviderMessages(req.Messages)...),
		goai.WithMaxSteps(req.MaxSteps),
	}
	// 请求级 provider options：useResponsesAPI 与 reasoning_effort 必须合并
	// 为一张 map（goai 的 WithProviderOptions 是整体覆盖，多次调用互相覆盖）。
	po := map[string]any{}
	// Responses API 是请求级开关（goai 的 openai provider 默认开），
	// 只在 type = openai.responses 时显式开启。
	if model.resp {
		po["useResponsesAPI"] = true
	}
	if req.ReasoningEffort != "" {
		po["reasoning_effort"] = req.ReasoningEffort
	}
	if len(po) > 0 {
		opts = append(opts, goai.WithProviderOptions(po))
	}
	if len(req.Tools) > 0 {
		opts = append(opts, goai.WithTools(req.Tools...))
	}
	if req.Hooks.BeforeToolExecute != nil {
		opts = append(opts, goai.WithOnBeforeToolExecute(func(info goai.BeforeToolExecuteInfo) goai.BeforeToolExecuteResult {
			res := req.Hooks.BeforeToolExecute(BeforeToolExecuteInfo{
				Ctx:    info.Ctx,
				CallID: info.ToolCallID,
				Name:   info.ToolName,
				Input:  rawToMap(info.Input),
			})
			return goai.BeforeToolExecuteResult{Skip: res.Skip, Result: res.Result, Error: res.Error}
		}))
	}
	if req.Hooks.ToolStart != nil {
		opts = append(opts, goai.WithOnToolCallStart(func(info goai.ToolCallStartInfo) {
			req.Hooks.ToolStart(ToolStartInfo{CallID: info.ToolCallID, Name: info.ToolName, Input: rawToMap(info.Input)})
		}))
	}
	if req.Hooks.ToolResult != nil {
		opts = append(opts, goai.WithOnToolCall(func(info goai.ToolCallInfo) {
			req.Hooks.ToolResult(ToolResultInfo{
				CallID: info.ToolCallID,
				Name:   info.ToolName,
				Output: info.Output,
				Error:  info.Error,
			})
		}))
	}

	ts, err := goai.StreamText(ctx, model.lm, opts...)
	if err != nil {
		return nil, err
	}
	return &goaiStream{ts: ts}, nil
}

// Chunks 返回转换后的流式增量通道；消费完毕后调用 Err/Usage 取结果。
func (s *goaiStream) Chunks() <-chan Chunk {
	out := make(chan Chunk, 64)
	go func() {
		defer close(out)
		for ch := range s.ts.Stream() {
			switch ch.Type {
			case provider.ChunkText:
				out <- Chunk{Text: ch.Text}
			case provider.ChunkToolCall:
				out <- Chunk{ToolCall: &ToolCall{
					ID:    ch.ToolCallID,
					Name:  ch.ToolName,
					Input: rawToMap(json.RawMessage(ch.ToolInput)),
				}}
			case provider.ChunkFinish:
				u := ch.Usage
				out <- Chunk{Finish: true, Usage: Usage{
					InputTokens:  u.InputTokens,
					OutputTokens: u.OutputTokens,
					TotalTokens:  u.TotalTokens,
				}}
			case provider.ChunkError:
				out <- Chunk{Err: ch.Error}
			}
			// ChunkToolCallDelta / ChunkToolCallStreamStart 忽略：
			// goai 保证最终会发出完整的 ChunkToolCall。
		}
	}()
	return out
}

// Err 返回流错误（消费完 Chunks 后调用）。
func (s *goaiStream) Err() error {
	return s.ts.Err()
}

// rawToMap 把 JSON 原始参数转为 map；解析失败返回空 map。
func rawToMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	return m
}
