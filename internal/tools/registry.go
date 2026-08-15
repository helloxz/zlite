// Package tools 实现 zlite 的工具函数（阅读/搜索/网络/shell 等）。
//
// 每个工具由 goai.NewTool[In] 构造（输入 struct 自动生成 JSON Schema），
// 注册表附带权限元数据（Mode 可见性 + NeedApprove 确认），
// 由 agent 按当前模式过滤后交给 llm 层注入模型。
package tools

import (
	"context"
	"encoding/json"
	"path"
	"strings"

	"github.com/zendev-sh/goai"
)

// Mode 是工具可见性模式（与 config.ModePlan/ModeBuild 字符串一致）。
type Mode string

const (
	ModePlan  Mode = "plan"
	ModeBuild Mode = "build"
)

// MaxOutputBytes 是所有工具输出的统一上限（防止上下文爆炸）。
const MaxOutputBytes = 64 * 1024

// Tool 是注册表中的一个工具。
type Tool struct {
	GoAITool goai.Tool
	// Modes 是工具可见的模式集合（plan/build 均可见时包含两者）。
	// 同名工具可按模式注册不同实现（如 run_command 的只读版/全量版）。
	Modes []Mode
	// NeedApprove 返回是否需要人工确认及确认摘要（nil 表示无需确认）。
	NeedApprove func(input map[string]any) (bool, string)
}

// Registry 是工具注册表。
type Registry struct {
	cwd     string
	confirm []string // 危险命令确认清单（build 模式 run_command 用）
	tools   []Tool
}

// Options 是 New 的配置项。
type Options struct {
	// ConfirmCommands 是 build 模式下需要确认的危险命令清单。
	ConfirmCommands []string
	// PlanExtraCommands 是用户追加到 plan 模式只读白名单的命令名。
	PlanExtraCommands []string
	// WebSearch 是否注册 web_search 工具（由上层按 config [tools] 段传入）。
	WebSearch bool
	// WebFetch 是否注册 web_fetch 工具（由上层按 config [tools] 段传入）。
	WebFetch bool
}

// New 创建注册表并注册内置工具。
// cwd 是工具执行的基准目录；opts 携带危险命令清单、plan 只读白名单追加项
// 与 web_search / web_fetch 开关（false 时不注册对应工具，plan/build 均不可见）。
func New(cwd string, opts Options) *Registry {
	r := &Registry{cwd: cwd, confirm: opts.ConfirmCommands}
	r.register(readFileTool(cwd))
	r.register(grepTool(cwd))
	r.register(globTool(cwd))
	if opts.WebFetch {
		r.register(webFetchTool())
	}
	if opts.WebSearch {
		r.register(webSearchTool())
	}
	r.register(runCommandPlanTool(cwd, opts.PlanExtraCommands))
	r.register(runCommandBuildTool(cwd, opts.ConfirmCommands))
	r.register(writeFileTool(cwd))
	r.register(editFileTool(cwd))
	r.register(deleteTool(cwd))
	return r
}

func (r *Registry) register(t Tool) {
	// 统一包装：所有工具输出经 truncate 截断，防止上下文爆炸。
	inner := t.GoAITool.Execute
	t.GoAITool.Execute = func(ctx context.Context, raw json.RawMessage) (string, error) {
		out, err := inner(ctx, raw)
		return truncate(out), err
	}
	r.tools = append(r.tools, t)
}

// Register 注册额外工具（二期新增工具或测试注入用）。
func (r *Registry) Register(t Tool) {
	r.register(t)
}

// Has 判断工具名是否已注册（MCP 工具注册前的重名检查）。
func (r *Registry) Has(name string) bool {
	for _, t := range r.tools {
		if t.GoAITool.Name == name {
			return true
		}
	}
	return false
}

// ForMode 返回指定模式下可见的 goai 工具列表（直接供 llm.StreamRequest.Tools 使用）。
func (r *Registry) ForMode(m Mode) []goai.Tool {
	out := make([]goai.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		if modeIn(t.Modes, m) {
			out = append(out, t.GoAITool)
		}
	}
	return out
}

// modeIn 判断模式是否在可见模式集合中。
func modeIn(modes []Mode, m Mode) bool {
	for _, mm := range modes {
		if mm == m {
			return true
		}
	}
	return false
}

// NeedApproveFor 查询工具是否需要确认（agent 在工具执行前调用）。
func (r *Registry) NeedApproveFor(name string, input map[string]any) (bool, string) {
	for _, t := range r.tools {
		if t.GoAITool.Name == name && t.NeedApprove != nil {
			return t.NeedApprove(input)
		}
	}
	return false, ""
}

// truncate 统一截断工具输出到 MaxOutputBytes。
// 保留头部与尾部各一段（尾部保留最近信息，如截断提示），中间标记截断。
func truncate(s string) string {
	if len(s) <= MaxOutputBytes {
		return s
	}
	const tailLen = 200
	headLen := MaxOutputBytes - tailLen
	head := s[:headLen]
	tail := s[len(s)-tailLen:]
	return head + "\n...[truncated]\n" + tail
}

// resolvePath 把工具参数中的相对路径解析为基于 cwd 的路径。
func resolvePath(cwd, p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return p
	}
	if cwd == "" {
		return p
	}
	return path.Join(cwd, p)
}
