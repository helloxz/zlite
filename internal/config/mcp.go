// Package config 的 MCP 部分：config.toml 的 [mcp] 段与 ~/.zlite/mcp.json 解析。
//
// MCP server 配置使用官方生态通用的 JSON 格式（Claude Code / Cursor 的
// mcpServers 约定）：~/.zlite/mcp.json 一个文件包含全部 server，网上教程的
// 配置片段可直接粘贴，零转换。TOML 目录模式已移除（不再兼容）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MCP 相关常量与默认值。
const (
	// DefaultMCPFile 是默认 MCP 配置文件（~/.zlite/mcp.json）。
	DefaultMCPFile = "~/.zlite/mcp.json"
	// DefaultMaxMCPServers 是默认同时启用的 server 数量上限：
	// 超出按 server 名排序丢弃（工具注入过多会稀释模型工具选择）。
	DefaultMaxMCPServers = 5
	// DefaultMaxToolsPerServer 是单个 server 注入工具数上限。
	DefaultMaxToolsPerServer = 20

	// transport 类型（官方 type 字段取值）。
	MCPTransportStdio = "stdio"
	MCPTransportHTTP  = "http"
	MCPTransportSSE   = "sse"
)

// MCPCfg 是 config.toml 的 [mcp] 段。
type MCPCfg struct {
	// Enabled 一键开关（缺省 true）。
	Enabled bool `mapstructure:"enabled"`
	// File 是 server 配置文件的路径（官方 mcpServers JSON 格式）。
	// 缺省 ~/.zlite/mcp.json；支持 ~ 展开。
	File string `mapstructure:"file"`
	// MaxServers 同时启用的 server 上限（缺省 5，超出按 server 名排序丢弃并警告）。
	MaxServers int `mapstructure:"max_servers"`
	// MaxToolsPerServer 单 server 注入工具数上限（缺省 20）。
	MaxToolsPerServer int `mapstructure:"max_tools_per_server"`
}

// MCPServer 是一个 MCP server 的配置（mcp.json 单个条目的解析结果）。
type MCPServer struct {
	// Name 是 mcpServers 对象里的 key（server 名）。
	Name string
	// Enabled 是否启用（disabled: true 的 server 跳过，等价于删除条目）。
	Enabled bool
	// Transport: stdio | http | sse（缺省 stdio）。
	Transport string
	// Command/Args: stdio 启动命令与参数（command 须可执行，连接前预检）。
	Command string
	Args    []string
	// Env: stdio 子进程附加环境变量（合并到当前进程环境；值支持 ${VAR} 展开）。
	Env map[string]string
	// URL: http/sse 端点。
	URL string
	// Headers: http/sse 请求头（值支持 ${VAR} 展开）。
	Headers map[string]string
	// AutoApprove: 免确认的工具名白名单（匹配 MCP server 返回的原始工具名）。
	// ["*"] = 信任该 server 全部工具；nil/空 = 全部工具每次调用需人工确认。
	AutoApprove []string
	// Path 配置文件路径（错误提示用）。
	Path string
}

// mcpFile 是 mcp.json 的顶层结构（官方 mcpServers 约定）。
type mcpFile struct {
	McpServers map[string]mcpServerEntry `json:"mcpServers"`
}

// mcpServerEntry 是单个 server 条目（Claude Code / Cursor 生态格式）。
// 未知字段由 json.Unmarshal 默认忽略，保证与其他客户端配置互相兼容。
type mcpServerEntry struct {
	Type        string            `json:"type"` // stdio | http | sse（缺省 stdio）
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Disabled    bool              `json:"disabled"`
	AutoApprove []string          `json:"autoApprove"`
}

// mcpNamePattern 校验 server 名：字母数字下划线连字符。
// 名字会拼进工具名（<server>_<tool>），须避免非法字符。
var mcpNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// envInlinePattern 匹配串内 ${VAR} 引用（支持拼接，如 "Bearer ${TOKEN}"）。
var envInlinePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// envPrefixedPattern 匹配 ${env:VAR}（Claude Code / Cursor 常见语法，等价 ${VAR}）。
var envPrefixedPattern = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// inputPlaceholderPattern 匹配 ${input:xxx}（宿主交互输入占位，zlite 不支持）。
var inputPlaceholderPattern = regexp.MustCompile(`\$\{input:[^}]*\}`)

// LoadMCPServers 解析 MCP 配置文件（缺省 ~/.zlite/mcp.json），
// 返回 MCPServer 列表（按 server 名排序，顺序可预期）。
//
// 文件不存在返回空集（未配置 MCP 是正常状态）。JSON 语法错误返回 error；
// 单个 server 条目非法（缺 command/url、type 不认识、环境变量缺失、
// ${input:} 占位等）只产生 warning 并跳过，不影响其他条目。
func LoadMCPServers(file string) ([]MCPServer, []string, error) {
	if file == "" {
		file = DefaultMCPFile
	}
	file, err := expandHomeDir(file)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil // 未配置 MCP：正常空集
		}
		return nil, nil, fmt.Errorf("read MCP config file failed: %w", err)
	}
	var f mcpFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("parse MCP config %s failed: %w", file, err)
	}

	// 对象 key 无序：排序保证确定性（max_servers 截断依赖顺序）
	names := make([]string, 0, len(f.McpServers))
	for name := range f.McpServers {
		names = append(names, name)
	}
	sort.Strings(names)

	var servers []MCPServer
	var warnings []string
	for _, name := range names {
		s, warn, ok := buildServer(name, file, f.McpServers[name])
		if !ok {
			if warn != "" {
				warnings = append(warnings, fmt.Sprintf("mcp: server %q: %s (skipped)", name, warn))
			}
			continue // 禁用或非法：均不保留
		}
		servers = append(servers, s)
	}
	return servers, warnings, nil
}

// buildServer 校验并构造 MCPServer。
// ok=false 表示不保留（warn 非空 = 配置非法；warn 空 = 用户主动禁用）。
func buildServer(name, path string, e mcpServerEntry) (s MCPServer, warn string, ok bool) {
	if !mcpNamePattern.MatchString(name) {
		return MCPServer{}, fmt.Sprintf("invalid server name %q (use letters, digits, _ or -)", name), false
	}
	if e.Disabled {
		return MCPServer{}, "", false // 已禁用：静默剔除
	}
	transport := e.Type
	if transport == "" {
		transport = MCPTransportStdio
	}
	switch transport {
	case MCPTransportStdio, MCPTransportHTTP, MCPTransportSSE:
	default:
		return MCPServer{}, fmt.Sprintf("invalid type %q (stdio | http | sse)", e.Type), false
	}

	command, args, warn := resolveCommand(e)
	if warn != "" {
		return MCPServer{}, warn, false
	}
	if transport == MCPTransportStdio && command == "" {
		return MCPServer{}, "type=stdio requires command", false
	}
	if transport != MCPTransportStdio && strings.TrimSpace(e.URL) == "" {
		return MCPServer{}, fmt.Sprintf("type=%s requires url", transport), false
	}

	s = MCPServer{
		Name:        name,
		Enabled:     true,
		Transport:   transport,
		Command:     command,
		Args:        args,
		Env:         e.Env,
		URL:         e.URL,
		Headers:     e.Headers,
		AutoApprove: e.AutoApprove,
		Path:        path,
	}
	// ${VAR} / ${env:VAR} 展开（env/headers，支持串内拼接）：
	// 变量缺失或含 ${input:} 占位视为配置错误（server 跳过）
	for k, v := range s.Env {
		ev, err := expandEnvInline(v)
		if err != nil {
			return MCPServer{}, fmt.Sprintf("env.%s: %v", k, err), false
		}
		s.Env[k] = ev
	}
	for k, v := range s.Headers {
		ev, err := expandEnvInline(v)
		if err != nil {
			return MCPServer{}, fmt.Sprintf("headers.%s: %v", k, err), false
		}
		s.Headers[k] = ev
	}
	return s, "", true
}

// resolveCommand 解析启动命令：
//   - command + args 数组：直接使用；
//   - command 为整串（含空格，网上常见 "npx -y pkg url"）：按空白拆分为
//     command + args；含引号时无法安全拆分，返回警告。
func resolveCommand(e mcpServerEntry) (command string, args []string, warn string) {
	if e.Command == "" {
		return "", nil, ""
	}
	if len(e.Args) > 0 {
		return e.Command, e.Args, ""
	}
	if !strings.ContainsAny(e.Command, " \t") {
		return e.Command, nil, ""
	}
	fields := strings.Fields(e.Command)
	for _, f := range fields {
		if strings.ContainsAny(f, `"'`) {
			return "", nil, "command contains quotes and cannot be split automatically; use command + args array form"
		}
	}
	if len(fields) == 0 {
		return "", nil, "empty command"
	}
	return fields[0], fields[1:], ""
}

// expandEnvInline 展开字符串中的 ${VAR} 与 ${env:VAR} 引用（支持拼接）。
// 任一变量缺失返回错误；含 ${input:xxx} 占位返回错误（zlite 无输入机制）。
func expandEnvInline(s string) (string, error) {
	if inputPlaceholderPattern.MatchString(s) {
		return "", fmt.Errorf("${input:...} placeholder is not supported; replace it with ${VAR} or a literal value")
	}
	// ${env:VAR} → 与 ${VAR} 等价展开
	s = envPrefixedPattern.ReplaceAllStringFunc(s, func(m string) string {
		name := envPrefixedPattern.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return "${ENVCHECK_MISSING:" + name + "}" // 哨兵：后续统一报缺失
		}
		return v
	})
	var firstErr error
	out := envInlinePattern.ReplaceAllStringFunc(s, func(m string) string {
		name := envInlinePattern.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("environment variable %s not set", name)
			}
			return m
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	// ${env:VAR} 阶段缺失的变量：哨兵转真实错误信息
	if i := strings.Index(out, "${ENVCHECK_MISSING:"); i >= 0 {
		end := strings.Index(out[i:], "}")
		if end > 0 {
			name := out[i+len("${ENVCHECK_MISSING:") : i+end]
			return "", fmt.Errorf("environment variable %s not set", name)
		}
	}
	return out, nil
}

// expandHomeDir 展开前导 ~ 为用户主目录（"~" 或 "~/..."）。
func expandHomeDir(dir string) (string, error) {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir failed: %w", err)
		}
		if dir == "~" {
			return home, nil
		}
		return filepath.Join(home, dir[2:]), nil
	}
	return dir, nil
}
