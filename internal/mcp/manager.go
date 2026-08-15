// Package mcp 实现 zlite 的 MCP 集成：加载 ~/.zlite/mcp/ 目录配置、
// 连接 MCP server（stdio/http/sse）、把远端工具桥接为 tools.Tool 注册进
// 工具注册表。
//
// 连接在后台进行（不阻塞启动）；首轮对话前由 Attach 幂等确保连接完成
// 并注册工具。单个 server 连接失败降级为警告并跳过，不影响其余 server
// 与 zlite 启动。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/tools"
	"github.com/zendev-sh/goai"
	goaimcp "github.com/zendev-sh/goai/mcp"
)

// connectTimeout 是 MCP 单次请求（initialize / tools/list 每页 / tools/call）的
// 超时上限。注意：它是每请求超时而非连接总时长上限——分页列表多页时
// 总时长可能达数倍；后台连接 + 首轮对话前等待的机制下通常无感。
const connectTimeout = 10 * time.Second

// conn 是一个已连接（或连接失败）的 MCP server 连接。
type conn struct {
	cfg      config.MCPServer
	client   *goaimcp.Client
	mcpTools []goaimcp.Tool // 已按 maxTools 截断
	err      error          // 连接失败原因（nil 表示成功）
}

// Manager 管理 MCP server 的连接与工具注册。
//
// 生命周期：New（同步解析配置，快）→ ConnectAsync（后台并行连接）→
// Attach（首轮对话前：等待连接完成 + 把工具注册进注册表，幂等）→ Close。
type Manager struct {
	maxServers int
	maxTools   int

	servers     []config.MCPServer // 解析后的 server 列表（已按 maxServers 截断）
	cfgWarnings []string          // 配置解析阶段警告（New 产生，TUI 启动前打印）
	connWarnings []string         // 连接阶段警告（Attach 后取走广播）

	mu       sync.Mutex
	done     chan struct{}             // 后台连接完成信号（nil 表示未启动）
	conns    []*conn                   // connectAll 的结果（与 servers 顺序一致）
	attached map[*tools.Registry]bool  // 已注册的注册表（每注册表只注册一次）

	// baseCtx 是连接期子进程的生命周期上下文：transport 的 exec.CommandContext
	// 绑定它，Close 时 cancel 终止连接中/已连接的全部 stdio 子进程（防孤儿）。
	baseCtx context.Context
	cancel  context.CancelFunc
}

// New 解析 MCP 配置目录并创建 Manager（不发起任何连接）。
// 目录不存在或未配置时为空集；maxServers/maxToolsPerServer <= 0 用默认值。
func New(dir string, maxServers, maxToolsPerServer int) *Manager {
	m := &Manager{
		maxServers: maxServers,
		maxTools:   maxToolsPerServer,
		attached:   make(map[*tools.Registry]bool),
	}
	m.baseCtx, m.cancel = context.WithCancel(context.Background())
	if m.maxServers <= 0 {
		m.maxServers = config.DefaultMaxMCPServers
	}
	if m.maxTools <= 0 {
		m.maxTools = config.DefaultMaxToolsPerServer
	}
	servers, warns, err := config.LoadMCPServers(dir)
	if err != nil {
		m.cfgWarnings = append(m.cfgWarnings, "mcp: "+err.Error())
		return m
	}
	m.cfgWarnings = append(m.cfgWarnings, warns...)
	// 数量上限：按文件名顺序丢弃后面的（顺序可预期，改名即可调整保留谁）
	if len(servers) > m.maxServers {
		dropped := make([]string, 0, len(servers)-m.maxServers)
		for _, s := range servers[m.maxServers:] {
			dropped = append(dropped, s.Name)
		}
		m.cfgWarnings = append(m.cfgWarnings, fmt.Sprintf(
			"mcp: %d servers exceed max_servers=%d, dropped: %s",
			len(servers), m.maxServers, strings.Join(dropped, ", ")))
		servers = servers[:m.maxServers]
	}
	m.servers = servers
	return m
}

// ConfigWarnings 返回配置解析阶段警告（启动时打印，TUI 尚未启动）。
func (m *Manager) ConfigWarnings() []string { return m.cfgWarnings }

// Enabled 判断是否配置了任何 server（无 server 时跳过连接与注入）。
func (m *Manager) Enabled() bool { return len(m.servers) > 0 }

// ConnectAsync 后台并行连接所有 server（幂等：只启动一次）。
func (m *Manager) ConnectAsync() {
	m.mu.Lock()
	if m.done != nil {
		m.mu.Unlock()
		return
	}
	m.done = make(chan struct{})
	m.mu.Unlock()
	go m.connectAll()
}

// EnsureConnected 等待连接完成（未启动则同步连接）。
// 单个 server 失败已降级（不返回错误）；仅在 ctx 取消时返回错误。
func (m *Manager) EnsureConnected(ctx context.Context) error {
	m.ConnectAsync()
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// connectAll 并行连接所有 server 并汇总结果（goroutine 写各自槽位，
// 汇总阶段一次性上锁，避免连接期间反复竞争）。
func (m *Manager) connectAll() {
	conns := make([]*conn, len(m.servers))
	var wg sync.WaitGroup
	for i, s := range m.servers {
		wg.Add(1)
		go func(i int, s config.MCPServer) {
			defer wg.Done()
			conns[i] = m.connectOne(s)
		}(i, s)
	}
	wg.Wait()

	var warns []string
	for _, c := range conns {
		if c.err != nil {
			warns = append(warns, fmt.Sprintf("mcp: server %q connect failed: %v", c.cfg.Name, c.err))
		}
	}
	m.mu.Lock()
	m.conns = conns
	m.connWarnings = append(m.connWarnings, warns...)
	m.mu.Unlock()
	close(m.done)
}

// connectOne 连接单个 server 并拉取工具列表。
// 注意：transport 的 exec.CommandContext 把传入 ctx 绑定为子进程生命周期，
// 取消会杀死子进程——因此连接期必须用长期 ctx，超时交给 client 的
// WithRequestTimeout（请求级超时，不杀子进程）。
func (m *Manager) connectOne(s config.MCPServer) *conn {
	c := &conn{cfg: s}
	client, err := buildClient(s)
	if err != nil {
		c.err = err
		return c
	}
	if err := client.Connect(m.baseCtx); err != nil {
		_ = client.Close()
		c.err = err
		return c
	}
	allTools, err := listAllTools(m.baseCtx, client)
	if err != nil {
		_ = client.Close()
		c.err = err
		return c
	}
	if len(allTools) > m.maxTools {
		m.mu.Lock()
		m.connWarnings = append(m.connWarnings, fmt.Sprintf(
			"mcp: server %q exposes %d tools, capped at max_tools_per_server=%d",
			s.Name, len(allTools), m.maxTools))
		m.mu.Unlock()
		allTools = allTools[:m.maxTools]
	}
	c.client = client
	c.mcpTools = allTools
	return c
}

// buildClient 按配置构造 goai MCP client。
func buildClient(s config.MCPServer) (*goaimcp.Client, error) {
	var tr goaimcp.Transport
	switch s.Transport {
	case config.MCPTransportStdio:
		// 启动前预检可执行文件：报清晰错误而非子进程神秘退出
		if _, err := exec.LookPath(s.Command); err != nil {
			return nil, fmt.Errorf("executable %q not found: %v", s.Command, err)
		}
		tr = goaimcp.NewStdioTransport(s.Command, s.Args, goaimcp.WithStdioEnv(s.Env))
	case config.MCPTransportHTTP:
		tr = goaimcp.NewHTTPTransport(s.URL, goaimcp.WithHTTPHeaders(s.Headers))
	case config.MCPTransportSSE:
		tr = goaimcp.NewSSETransport(s.URL, goaimcp.WithSSEHeaders(s.Headers))
	default:
		return nil, fmt.Errorf("unsupported transport %q", s.Transport)
	}
	return goaimcp.NewClient(s.Name, "1.0.0",
		goaimcp.WithTransport(tr),
		goaimcp.WithRequestTimeout(connectTimeout)), nil
}

// listAllTools 分页拉取全部工具。
func listAllTools(ctx context.Context, client *goaimcp.Client) ([]goaimcp.Tool, error) {
	var all []goaimcp.Tool
	cursor := ""
	for {
		res, err := client.ListTools(ctx, &goaimcp.ListParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
}

// Attach 确保连接完成并把全部工具注册进 reg（幂等：每注册表只注册一次）。
// 单个 server 连接失败已降级（只注册成功的）；工具名冲突跳过并警告。
func (m *Manager) Attach(ctx context.Context, reg *tools.Registry) error {
	if !m.Enabled() {
		return nil // 无 server：直接放行，零开销
	}
	if err := m.EnsureConnected(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.attached[reg] {
		return nil
	}
	m.attached[reg] = true
	for _, c := range m.conns {
		if c.client == nil {
			continue // 连接失败，跳过
		}
		for _, t := range c.mcpTools {
			tool := m.bridgeTool(c, t)
			if reg.Has(tool.GoAITool.Name) {
				m.connWarnings = append(m.connWarnings, fmt.Sprintf(
					"mcp: tool name conflict %q (server %q), skipped",
					tool.GoAITool.Name, c.cfg.Name))
				continue
			}
			reg.Register(tool)
		}
	}
	return nil
}

// bridgeTool 把 MCP 工具桥接为 tools.Tool：工具名加 <server>_ 前缀
// （防跨 server 重名、来源可识别）；权限按配置包装
// （approve=all → 每次调用需人工确认）。
func (m *Manager) bridgeTool(c *conn, t goaimcp.Tool) tools.Tool {
	name := c.cfg.Name + "_" + t.Name
	tt := tools.Tool{
		GoAITool: goai.Tool{
			Name:        name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Execute: func(ctx context.Context, raw json.RawMessage) (string, error) {
				args := make(map[string]any)
				if len(raw) > 0 {
					if err := json.Unmarshal(raw, &args); err != nil {
						return "", err
					}
				}
				res, err := c.client.CallTool(ctx, t.Name, args)
				if err != nil {
					return "", err
				}
				return goaimcp.FormatContent(res.Content, res.IsError), nil
			},
		},
		Modes: make([]tools.Mode, 0, len(c.cfg.Modes)),
	}
	for _, mm := range c.cfg.Modes {
		tt.Modes = append(tt.Modes, tools.Mode(mm))
	}
	if c.cfg.Approve == config.MCPApproveAll {
		tt.NeedApprove = func(map[string]any) (bool, string) {
			return true, fmt.Sprintf("call MCP tool %q on server %q", name, c.cfg.Name)
		}
	}
	return tt
}

// TakeWarnings 取走并清空连接阶段警告（Attach 后广播一次）。
func (m *Manager) TakeWarnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.connWarnings
	m.connWarnings = nil
	return w
}

// Close 关闭全部连接（终止 stdio 子进程）。
// 先取消 baseCtx：连接中/已连接的 stdio 子进程（exec.CommandContext 绑定
// baseCtx）随之终止，避免未完成连接留下孤儿进程；再显式 Close 已连接 client。
// 连接完成后调用时 cancel 无副作用（子进程此时由 client.Close 统一回收）。
func (m *Manager) Close() error {
	m.cancel()
	m.mu.Lock()
	conns := m.conns
	m.mu.Unlock()
	var errs []string
	for _, c := range conns {
		if c.client != nil {
			if err := c.client.Close(); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("mcp: close failed: %s", strings.Join(errs, "; "))
	}
	return nil
}
