// Package config 负责加载 ~/.zlite/config.toml。
//
// 使用 viper 解析 toml，转换为类型化 struct 后供业务代码使用；
// 业务代码不直接接触 viper。配置结构为多 provider 数组（[[providers]]），
// 模型以 "provider_name/model_name" 引用，渠道名取自配置 name 字段。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/joho/godotenv"
	"github.com/spf13/viper"
)

// ErrConfigNotFound 表示配置文件不存在。
var ErrConfigNotFound = errors.New("config file not found")

// 模式与类型常量（与 agent 包解耦，避免循环依赖）。
const (
	ModePlan  = "plan"
	ModeBuild = "build"

	// Provider type 取值：厂商[.协议]。
	// 新增厂商直接在此追加枚举，llm.buildModel 里加一行分派即可。
	TypeOpenAIChat      = "openai.chat"      // OpenAI Chat Completions（默认，兼容一切自定义端点）
	TypeOpenAIResponses = "openai.responses" // OpenAI Responses API（要求端点支持 /responses）
	TypeAnthropic       = "anthropic"        // Anthropic Messages API（/v1/messages）
)

// 默认值。
const (
	defaultMode     = ModePlan
	defaultMaxSteps = 16
	defaultType     = TypeOpenAIChat
)

// defaultConfirmCommands 是危险命令确认清单（决策 D4）。
var defaultConfirmCommands = []string{"rm", "mv", "dd", "mkfs", "sudo", "chmod"}

// Config 是完整配置。
type Config struct {
	Providers []Provider `mapstructure:"providers"`
	Agent     AgentCfg   `mapstructure:"agent"`
	Shell     ShellCfg   `mapstructure:"shell"`
	MCP       MCPCfg     `mapstructure:"mcp"`
}

// Provider 描述一个模型渠道。
// Name 是渠道名（唯一、不含 /，作为模型引用 "provider_name/model_name" 的前缀）；
// Type 取值见 Type* 常量（厂商.协议），缺省 openai.chat；Models 至少一个。
type Provider struct {
	Name    string   `mapstructure:"name"`
	Type    string   `mapstructure:"type"`
	BaseURL string   `mapstructure:"base_url"`
	APIKey  string   `mapstructure:"api_key"` // 支持 ${ENV} 展开（可放 ~/.zlite/.env）
	Models  []string `mapstructure:"models"`
}

// AgentCfg 是 agent 行为配置。
type AgentCfg struct {
	Mode        string `mapstructure:"mode"` // plan | build
	AutoApprove bool   `mapstructure:"auto_approve"`
	MaxSteps    int    `mapstructure:"max_steps"`
	// LoadAgentsMD 自动加载项目根 AGENTS.md 注入系统提示词（默认开启）。
	LoadAgentsMD bool `mapstructure:"load_agents_md"`
	// DefaultModel 默认模型引用（provider_name/model_name），TUI/ACP 初始模型用；
	// 缺省时取第一个渠道的第一个模型。
	DefaultModel string `mapstructure:"default_model"`
}

// ShellCfg 是 shell 工具配置。
type ShellCfg struct {
	// ConfirmCommands 是 build 模式下需要确认的危险命令黑名单。
	ConfirmCommands []string `mapstructure:"confirm_commands"`
	// PlanExtraCommands 追加到 plan 模式只读命令白名单的命令名
	// （与内置白名单合并、去重；如 "python3"、"kubectl"）。默认空。
	PlanExtraCommands []string `mapstructure:"plan_extra_commands"`
}

// DefaultConfig 返回带默认值的配置。
func DefaultConfig() *Config {
	return &Config{
		Agent: AgentCfg{
			Mode:         defaultMode,
			AutoApprove:  false,
			MaxSteps:     defaultMaxSteps,
			LoadAgentsMD: true,
		},
		Shell: ShellCfg{ConfirmCommands: append([]string(nil), defaultConfirmCommands...)},
		MCP: MCPCfg{
			Dir:               DefaultMCPDir,
			Enabled:           true,
			MaxServers:        DefaultMaxMCPServers,
			MaxToolsPerServer: DefaultMaxToolsPerServer,
		},
	}
}

// Load 读取配置文件。
//
// 读取前会先加载同目录下的 .env 文件（不存在则忽略），其中的变量
// （如 ZLITE_DEFAULT_API_KEY）可被 api_key 的 ${VAR} 引用。已存在的环境变量
// 优先于 .env（godotenv 默认不覆盖），shell 里 export 与 .env 可共存。
//
// 文件不存在时返回 ErrConfigNotFound（上层可调用 WriteTemplate 生成模板后提示用户）。
// APIKey 中的 ${VAR} 会在读取后展开；展开失败返回错误。
func Load(path string) (*Config, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("检查配置文件失败: %w", err)
	}

	// 自动加载与配置文件同目录的 .env（~/.zlite/.env），不存在则忽略。
	dotEnv := filepath.Join(filepath.Dir(path), ".env")
	if _, err := os.Stat(dotEnv); err == nil {
		if err := godotenv.Load(dotEnv); err != nil {
			return nil, fmt.Errorf("加载 %s 失败: %w", dotEnv, err)
		}
	}

	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}

	cfg := DefaultConfig()
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}

	for i := range cfg.Providers {
		expanded, err := expandEnv(cfg.Providers[i].APIKey)
		if err != nil {
			return nil, fmt.Errorf("providers[%d].api_key: %w", i, err)
		}
		cfg.Providers[i].APIKey = expanded
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DefaultPath 返回默认配置文件路径 ~/.zlite/config.toml。
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录失败: %w", err)
	}
	return filepath.Join(home, ".zlite", "config.toml"), nil
}

// SplitModelSpec 把模型引用 "provider_name/model_name" 拆为渠道名与模型名：
// 第一个 / 之前是渠道名（渠道名校验不含 /），其余部分（可含 /）是模型名，
// 因此支持 "deepseek/deepseek-ai/deepseek-chat" 这类模型名自带斜杠的引用。
func SplitModelSpec(spec string) (provider, model string, err error) {
	i := strings.IndexByte(spec, '/')
	if i <= 0 || i == len(spec)-1 {
		return "", "", fmt.Errorf("非法模型引用: %q（应为 provider_name/model_name）", spec)
	}
	return spec[:i], spec[i+1:], nil
}

// ResolveProvider 按渠道名查找渠道（返回副本，避免外部修改内部状态）。
func (c *Config) ResolveProvider(name string) (*Provider, error) {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i], nil
		}
	}
	return nil, fmt.Errorf("未找到渠道 %q（已配置: %s）", name, providerNames(c.Providers))
}

// providerNames 返回渠道名列表（错误提示用）。
func providerNames(ps []Provider) string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}

// ResolveModelSpec 按模型引用 "provider_name/model_name" 解析渠道与模型，
// 并校验模型在该渠道的 models 列表内（配置变更后引用可能失效）。
func (c *Config) ResolveModelSpec(spec string) (*Provider, string, error) {
	name, model, err := SplitModelSpec(spec)
	if err != nil {
		return nil, "", err
	}
	p, err := c.ResolveProvider(name)
	if err != nil {
		return nil, "", err
	}
	if !slices.Contains(p.Models, model) {
		return nil, "", fmt.Errorf("模型 %q 不在渠道 %q 的 models 列表中", model, name)
	}
	return p, model, nil
}

// DefaultModelName 返回默认模型引用 "provider_name/model_name"：
// agent.default_model 优先（并校验可解析），缺省取第一个渠道的第一个模型。
func (c *Config) DefaultModelName() (string, error) {
	if len(c.Providers) == 0 {
		return "", errors.New("未配置任何 provider，请在配置文件的 [[providers]] 中填写渠道信息")
	}
	if c.Agent.DefaultModel != "" {
		if _, _, err := c.ResolveModelSpec(c.Agent.DefaultModel); err != nil {
			return "", fmt.Errorf("agent.default_model: %w", err)
		}
		return c.Agent.DefaultModel, nil
	}
	p := c.Providers[0]
	return p.Name + "/" + p.Models[0], nil
}

// RestoreModelSpec 还原会话记录的模型引用：记录为新格式（provider_name/model_name）
// 直接校验；旧格式（纯模型名）用会话记录的渠道名拼回校验；均无法解析返回错误
// （调用方回退默认模型）。配置变更后记录可能失效。
func (c *Config) RestoreModelSpec(providerName, model string) (string, error) {
	if model == "" {
		return "", errors.New("会话未记录模型")
	}
	if _, _, err := c.ResolveModelSpec(model); err == nil {
		return model, nil
	}
	if providerName != "" {
		spec := providerName + "/" + model
		if _, _, err := c.ResolveModelSpec(spec); err == nil {
			return spec, nil
		}
	}
	return "", fmt.Errorf("会话记录的模型 %q 无法解析", model)
}

// AllModels 返回全部渠道的模型引用扁平列表 "provider_name/model_name"，
// 按配置顺序排列（TUI /switch 与 ACP 模型选项的数据源）。
func (c *Config) AllModels() []string {
	var out []string
	for _, p := range c.Providers {
		for _, m := range p.Models {
			out = append(out, p.Name+"/"+m)
		}
	}
	return out
}

// validate 校验全局配置项：agent/session 段 + 全部渠道（type/base_url/models/name）。
func (c *Config) validate() error {
	switch c.Agent.Mode {
	case ModePlan, ModeBuild:
	default:
		return fmt.Errorf("agent.mode 非法: %q（仅支持 %q / %q）", c.Agent.Mode, ModePlan, ModeBuild)
	}
	if c.Agent.MaxSteps <= 0 {
		return fmt.Errorf("agent.max_steps 必须为正整数，当前: %d", c.Agent.MaxSteps)
	}
	seen := make(map[string]bool, len(c.Providers))
	for i := range c.Providers {
		p := &c.Providers[i]
		idx := fmt.Sprintf("providers[%d]", i)
		if p.Name == "" {
			return fmt.Errorf("%s.name 未配置", idx)
		}
		if strings.Contains(p.Name, "/") {
			return fmt.Errorf("%s.name 不能包含 '/'（当前: %q），渠道名是模型引用 provider_name/model_name 的前缀", idx, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("渠道名重复: %q", p.Name)
		}
		seen[p.Name] = true
		typ := p.Type
		if typ == "" {
			typ = defaultType // 缺省 openai.chat
		}
		switch typ {
		case TypeOpenAIChat, TypeOpenAIResponses, TypeAnthropic:
		default:
			return fmt.Errorf("%s.type 暂不支持: %q（当前支持 %q / %q / %q）", idx, typ, TypeOpenAIChat, TypeOpenAIResponses, TypeAnthropic)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("%s.base_url 未配置", idx)
		}
		if len(p.Models) == 0 {
			return fmt.Errorf("%s.models 未配置（至少填写一个模型）", idx)
		}
		for _, m := range p.Models {
			if m == "" {
				return fmt.Errorf("%s.models 含空模型名", idx)
			}
		}
	}
	// default_model 引用必须可解析（渠道存在且模型在列表内）
	if c.Agent.DefaultModel != "" {
		if _, _, err := c.ResolveModelSpec(c.Agent.DefaultModel); err != nil {
			return fmt.Errorf("agent.default_model: %w", err)
		}
	}
	// MCP 段：数量上限须为正整数
	if c.MCP.MaxServers <= 0 {
		return fmt.Errorf("mcp.max_servers must be positive, got %d", c.MCP.MaxServers)
	}
	if c.MCP.MaxToolsPerServer <= 0 {
		return fmt.Errorf("mcp.max_tools_per_server must be positive, got %d", c.MCP.MaxToolsPerServer)
	}
	return nil
}

var envPattern = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// NeedsSetup 判断是否需要首次引导配置：
// 存在任一已配置（name 非空且 api_key 非空，展开后）的渠道即视为已配置。
// 注意：api_key = "${ZLITE_DEFAULT_API_KEY}" 且 .env 已设置时，展开后非空，视为已配置。
func (c *Config) NeedsSetup() bool {
	for _, p := range c.Providers {
		if p.Name != "" && p.APIKey != "" {
			return false
		}
	}
	return true
}

// SetupInput 是一次引导配置的输入（ApplySetup 的入参）。
type SetupInput struct {
	Type    string // openai.chat | openai.responses
	BaseURL string
	APIKey  string
	Models  []string
}

// SplitModels 把用户输入的模型列表拆为 []string：
// 支持中英文逗号（, ，）分隔，自动去除空白字符与空项。
func SplitModels(s string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`[,，]`).Split(s, -1) {
		m = strings.TrimSpace(m)
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}

// ApplySetup 把引导结果写入磁盘：
//   - api_key 追加/更新到配置文件同目录的 .env（ZLITE_DEFAULT_API_KEY=...，0600），
//     不覆盖 .env 中已有的其他变量；
//   - config.toml 只更新 [[providers]] 段（保留 agent/shell/tui/session 等其他段），
//     api_key 写 ${ZLITE_DEFAULT_API_KEY} 占位，密钥不落盘到配置。
//
// 配置文件不存在时（首次运行）直接创建。
func ApplySetup(path string, in SetupInput) error {
	switch in.Type {
	case TypeOpenAIChat, TypeOpenAIResponses:
	default:
		return fmt.Errorf("type 非法: %q（仅支持 %q / %q）", in.Type, TypeOpenAIChat, TypeOpenAIResponses)
	}
	if in.BaseURL == "" {
		return errors.New("base_url 不能为空")
	}
	if in.APIKey == "" {
		return errors.New("api_key 不能为空")
	}
	if len(in.Models) == 0 {
		return errors.New("models 不能为空")
	}

	if err := writeDotEnvKey(filepath.Join(filepath.Dir(path), ".env"), "ZLITE_DEFAULT_API_KEY", in.APIKey); err != nil {
		return err
	}

	v := viper.New()
	v.SetConfigFile(path)
	if _, err := os.Stat(path); err == nil {
		if err := v.ReadInConfig(); err != nil {
			return fmt.Errorf("读取配置失败: %w", err)
		}
	}
	v.Set("providers", []map[string]any{{
		"name":     "default",
		"type":     in.Type,
		"base_url": in.BaseURL,
		"api_key":  "${ZLITE_DEFAULT_API_KEY}",
		"models":   in.Models,
	}})
	if err := v.WriteConfigAs(path); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

// writeDotEnvKey 追加或更新 .env 中的 KEY=VALUE 行（保留其他行，权限 0600）。
func writeDotEnvKey(path, key, value string) error {
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		trimmed := strings.TrimRight(string(data), "\n")
		if trimmed != "" {
			lines = strings.Split(trimmed, "\n")
		}
	}
	prefix := key + "="
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			lines[i] = prefix + value
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, prefix+value)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("写入 .env 失败: %w", err)
	}
	return nil
}

// expandEnv 展开整串 ${VAR} 形式的环境变量引用；非该形式原样返回。
// 一期仅支持整串引用（不含拼接），保证实现简单可预期。
func expandEnv(s string) (string, error) {
	m := envPattern.FindStringSubmatch(s)
	if m == nil {
		return s, nil
	}
	val, ok := os.LookupEnv(m[1])
	if !ok || val == "" {
		return "", fmt.Errorf("环境变量 %s 未设置", m[1])
	}
	return val, nil
}

// WriteTemplate 将默认配置模板写入 path（自动创建父目录，权限 0600）。
// 文件已存在时返回错误，避免覆盖用户配置。
func WriteTemplate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("配置文件已存在: %s", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查配置文件失败: %w", err)
	}

	const tpl = `# zlite 配置文件
# 修改后需重启 zlite 生效；api_key 支持 ${ENV} 形式引用环境变量，
# 变量可写在 ~/.zlite/.env（推荐，避免密钥落盘）或 shell 环境里。

[[providers]]                    # 可配置多个渠道，模型以 provider_name/model_name 引用
  name = "default"               # 渠道名（唯一、不含 /）
  type = "openai.chat"           # 厂商.协议：openai.chat | openai.responses | anthropic
  base_url = "https://api.example.com/v1"
  api_key = "${ZLITE_DEFAULT_API_KEY}"   # 放 ~/.zlite/.env: ZLITE_DEFAULT_API_KEY=sk-...
  models = ["gpt-4o", "gpt-4o-mini"]   # 可多个

[agent]
  mode = "plan"                  # plan（只读）| build（可写）
  default_model = "default/gpt-4o"  # 默认模型（provider_name/model_name）；缺省取第一个渠道第一个模型
  auto_approve = false           # 信任模式（跳过危险命令确认）
  max_steps = 16                 # 单轮工具循环上限
  load_agents_md = true          # 自动加载项目根 AGENTS.md 注入系统提示词

[shell]
  confirm_commands = ["rm", "mv", "dd", "mkfs", "sudo", "chmod", "git", "git-push"]
  plan_extra_commands = []       # plan 模式额外放行命令（与内置只读白名单合并、去重）

[mcp]
  enabled = true                  # MCP 总开关
  dir = "~/.zlite/mcp"            # MCP server 配置目录（一 server 一个 toml 文件）
  max_servers = 5                 # 同时启用上限（超出按文件名顺序丢弃并警告）
  max_tools_per_server = 20       # 单个 server 注入工具数上限

# MCP server 配置放在 ~/.zlite/mcp/ 下，一 server 一个 toml 文件（文件名即 server 名），例如：
#
#   # ~/.zlite/mcp/github.toml
#   transport = "stdio"                        # stdio | http | sse（缺省 stdio）
#   command = "npx"                           # stdio 必填
#   args = ["-y", "@modelcontextprotocol/server-github"]
#   env = { GITHUB_PERSONAL_ACCESS_TOKEN = "${GITHUB_TOKEN}" }   # 支持 ${ENV} 引用
#   approve = "all"                           # all（每次调用需确认）| never（信任）
#
#   # ~/.zlite/mcp/remote.toml
#   transport = "http"                        # http | sse 需配置 url
#   url = "https://mcp.example.com"
#   headers = { Authorization = "Bearer ${TOKEN}" }
`

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimPrefix(tpl, "\n")), 0o600); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	return nil
}
