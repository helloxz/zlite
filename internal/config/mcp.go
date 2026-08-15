// Package config 的 MCP 部分：config.toml 的 [mcp] 段与 ~/.zlite/mcp/ 目录扫描。
//
// MCP server 配置为"一 server 一文件"：~/.zlite/mcp/<name>.toml（文件名即
// server 名，文件内不重复写）。单文件解析失败只产生 warning 并跳过，
// 不影响其他文件——第三方 server 配置五花八门，不能拖垮整体启动。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// MCP 相关常量与默认值。
const (
	// DefaultMCPDir 是默认 MCP 配置目录（~/.zlite/mcp/）。
	DefaultMCPDir = "~/.zlite/mcp"
	// DefaultMaxMCPServers 是默认同时启用的 server 数量上限：
	// 超出按文件名顺序丢弃（工具注入过多会稀释模型工具选择）。
	DefaultMaxMCPServers = 5
	// DefaultMaxToolsPerServer 是单个 server 注入工具数上限。
	DefaultMaxToolsPerServer = 20

	// transport 类型。
	MCPTransportStdio = "stdio"
	MCPTransportHTTP  = "http"
	MCPTransportSSE   = "sse"

	// approve 取值：all = 每次工具调用需人工确认（安全默认）；
	// never = 信任该 server 直接执行（用户显式声明）。
	MCPApproveAll   = "all"
	MCPApproveNever = "never"
)

// MCPCfg 是 config.toml 的 [mcp] 段。
type MCPCfg struct {
	// Dir 是 server 配置目录（一 server 一文件）；支持 ~ 展开。缺省 ~/.zlite/mcp。
	Dir string `mapstructure:"dir"`
	// Enabled 一键开关（缺省 true）。
	Enabled bool `mapstructure:"enabled"`
	// MaxServers 同时启用的 server 上限（缺省 5，超出按文件名顺序丢弃并警告）。
	MaxServers int `mapstructure:"max_servers"`
	// MaxToolsPerServer 单 server 注入工具数上限（缺省 20，超出丢弃并警告）。
	MaxToolsPerServer int `mapstructure:"max_tools_per_server"`
}

// MCPServer 是一个 MCP server 的配置（~/.zlite/mcp/<name>.toml 解析结果）。
// Name 取自文件名（去 .toml 后缀）。
type MCPServer struct {
	Name string
	// Enabled 是否启用（enabled = false 的 server 跳过，等价于删除文件）。
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
	// Approve: all（缺省，每次工具调用需确认）| never（信任直接执行）。
	Approve string
	// Modes: 可见模式（plan/build 子集；缺省双模式均可见）。
	Modes []string
	// Path 配置文件绝对路径（错误提示用）。
	Path string
}

// serverFile 是单个 server 配置文件的 TOML 结构。
// Enabled 用指针区分"未设置"与"false"（缺省 true）。
type serverFile struct {
	Enabled   *bool             `toml:"enabled"`
	Transport string            `toml:"transport"`
	Command   string            `toml:"command"`
	Args      []string          `toml:"args"`
	Env       map[string]string `toml:"env"`
	URL       string            `toml:"url"`
	Headers   map[string]string `toml:"headers"`
	Approve   string            `toml:"approve"`
	Modes     []string          `toml:"modes"`
}

// mcpNamePattern 校验 server 名（即文件名）：字母数字下划线连字符。
// 名字会拼进工具名（<server>_<tool>），须避免非法字符。
var mcpNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// envInlinePattern 匹配串内 ${VAR} 引用（支持拼接，如 "Bearer ${TOKEN}"）。
// 与 config.go 的 expandEnv（仅整串）互补：MCP headers/env 拼接是常见写法。
var envInlinePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvInline 展开字符串中所有 ${VAR} 引用；任一变量缺失返回错误。
func expandEnvInline(s string) (string, error) {
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
	return out, nil
}

// LoadMCPServers 扫描 dir 下的 *.toml（一 server 一文件，文件名即 name），
// 解析为 MCPServer 列表（按文件名排序，顺序可预期）。
//
// 目录不存在返回空集（未配置 MCP 是正常状态）；单文件解析失败只产生
// warning 并跳过；enabled=false 的 server 静默剔除。env/headers 中的
// ${VAR} 引用展开（支持串内拼接）；展开失败（环境变量未设置）该 server
// 跳过并警告。
func LoadMCPServers(dir string) ([]MCPServer, []string, error) {
	if dir == "" {
		dir = DefaultMCPDir
	}
	dir, err := expandHomeDir(dir)
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil // 未配置 MCP：正常空集
		}
		return nil, nil, fmt.Errorf("read MCP config dir failed: %w", err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var servers []MCPServer
	var warnings []string
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("mcp: read %s failed: %v", name, err))
			continue
		}
		var f serverFile
		if err := toml.Unmarshal(data, &f); err != nil {
			warnings = append(warnings, fmt.Sprintf("mcp: %s parse failed (skipped): %v", name, err))
			continue
		}
		s, warn, ok := buildServer(strings.TrimSuffix(name, ".toml"), path, f)
		if !ok {
			if warn != "" {
				warnings = append(warnings, fmt.Sprintf("mcp: %s: %s (skipped)", name, warn))
			}
			continue // 禁用或非法：均不保留
		}
		servers = append(servers, s)
	}
	return servers, warnings, nil
}

// buildServer 校验并构造 MCPServer。
// ok=false 表示不保留（warn 非空 = 配置非法；warn 空 = 用户主动禁用）。
func buildServer(name, path string, f serverFile) (s MCPServer, warn string, ok bool) {
	if !mcpNamePattern.MatchString(name) {
		return MCPServer{}, fmt.Sprintf("invalid server name %q (use letters, digits, _ or -)", name), false
	}
	if f.Enabled != nil && !*f.Enabled {
		return MCPServer{}, "", false // 已禁用：静默剔除（等价于删除文件）
	}
	transport := f.Transport
	if transport == "" {
		transport = MCPTransportStdio
	}
	switch transport {
	case MCPTransportStdio, MCPTransportHTTP, MCPTransportSSE:
	default:
		return MCPServer{}, fmt.Sprintf("invalid transport %q (stdio | http | sse)", f.Transport), false
	}
	if transport == MCPTransportStdio && strings.TrimSpace(f.Command) == "" {
		return MCPServer{}, "transport=stdio requires command", false
	}
	if transport != MCPTransportStdio && strings.TrimSpace(f.URL) == "" {
		return MCPServer{}, fmt.Sprintf("transport=%s requires url", transport), false
	}
	approve := f.Approve
	if approve == "" {
		approve = MCPApproveAll
	}
	switch approve {
	case MCPApproveAll, MCPApproveNever:
	default:
		return MCPServer{}, fmt.Sprintf("invalid approve %q (all | never)", f.Approve), false
	}
	modes := f.Modes
	if len(modes) == 0 {
		modes = []string{ModePlan, ModeBuild} // 缺省双模式可见
	}
	for _, m := range modes {
		if m != ModePlan && m != ModeBuild {
			return MCPServer{}, fmt.Sprintf("invalid mode %q (plan | build)", m), false
		}
	}

	s = MCPServer{
		Name:      name,
		Enabled:   true,
		Transport: transport,
		Command:   f.Command,
		Args:      f.Args,
		Env:       f.Env,
		URL:       f.URL,
		Headers:   f.Headers,
		Approve:   approve,
		Modes:     modes,
		Path:      path,
	}
	// ${VAR} 展开（env/headers，支持串内拼接）：任一展开失败视为配置错误
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
