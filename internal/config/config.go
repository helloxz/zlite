// Package config 负责加载 ~/.zlite/config.toml。
//
// 使用 viper 解析 toml，转换为类型化 struct 后供业务代码使用；
// 业务代码不直接接触 viper。配置结构按多 provider 数组设计（[[providers]]），
// 一期只取第一个，后期扩展多渠道时无需迁移配置格式。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	// 一期支持 OpenAI 系两种协议；未来新增厂商（如 anthropic/google）
	// 直接在此追加枚举，llm.BuildModel 里加一行分派即可。
	TypeOpenAIChat      = "openai.chat"      // OpenAI Chat Completions（默认，兼容一切自定义端点）
	TypeOpenAIResponses = "openai.responses" // OpenAI Responses API（要求端点支持 /responses）
)

// 默认值。
const (
	defaultMode        = ModePlan
	defaultMaxSteps    = 16
	defaultSessionKeep = 20
	defaultTheme       = "dark"
	defaultType        = TypeOpenAIChat
)

// defaultConfirmCommands 是危险命令确认清单（决策 D4）。
var defaultConfirmCommands = []string{"rm", "mv", "dd", "mkfs", "sudo", "chmod", "git", "git-push"}

// Config 是完整配置。
type Config struct {
	Providers []Provider `mapstructure:"providers"`
	Agent     AgentCfg   `mapstructure:"agent"`
	Shell     ShellCfg   `mapstructure:"shell"`
	TUI       TUISet     `mapstructure:"tui"`
	Session   SessionCfg `mapstructure:"session"`
}

// Provider 描述一个模型渠道。
// Type 取值见 Type* 常量（厂商.协议），缺省 openai.chat；
// Models 至少一个，默认使用第一个。
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
}

// ShellCfg 是 shell 工具配置。
type ShellCfg struct {
	ConfirmCommands []string `mapstructure:"confirm_commands"`
}

// TUISet 是 TUI 配置（一期仅预留）。
type TUISet struct {
	Theme string `mapstructure:"theme"`
}

// SessionCfg 是会话配置。
type SessionCfg struct {
	Keep int `mapstructure:"keep"`
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
		Shell:   ShellCfg{ConfirmCommands: append([]string(nil), defaultConfirmCommands...)},
		TUI:     TUISet{Theme: defaultTheme},
		Session: SessionCfg{Keep: defaultSessionKeep},
	}
}

// Load 读取配置文件。
//
// 读取前会先加载同目录下的 .env 文件（不存在则忽略），其中的变量
// （如 ZLITE_API_KEY）可被 api_key 的 ${VAR} 引用。已存在的环境变量
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

// DefaultProvider 返回第一个 provider 并校验其合法性（一期单渠道）。
func (c *Config) DefaultProvider() (*Provider, error) {
	if len(c.Providers) == 0 {
		return nil, errors.New("未配置任何 provider，请在配置文件的 [[providers]] 中填写渠道信息")
	}
	p := c.Providers[0] // 副本，避免外部修改内部状态
	if p.Type == "" {
		p.Type = defaultType // 缺省 openai.chat
	}
	switch p.Type {
	case TypeOpenAIChat, TypeOpenAIResponses:
	default:
		return nil, fmt.Errorf("providers[0].type 暂不支持: %q（当前支持 %q / %q）", p.Type, TypeOpenAIChat, TypeOpenAIResponses)
	}
	if p.BaseURL == "" {
		return nil, errors.New("providers[0].base_url 未配置")
	}
	if len(p.Models) == 0 {
		return nil, errors.New("providers[0].models 未配置（至少填写一个模型）")
	}
	for _, m := range p.Models {
		if m == "" {
			return nil, errors.New("providers[0].models 含空模型名")
		}
	}
	return &p, nil
}

// validate 校验全局配置项。
func (c *Config) validate() error {
	switch c.Agent.Mode {
	case ModePlan, ModeBuild:
	default:
		return fmt.Errorf("agent.mode 非法: %q（仅支持 %q / %q）", c.Agent.Mode, ModePlan, ModeBuild)
	}
	if c.Agent.MaxSteps <= 0 {
		return fmt.Errorf("agent.max_steps 必须为正整数，当前: %d", c.Agent.MaxSteps)
	}
	if c.Session.Keep <= 0 {
		return fmt.Errorf("session.keep 必须为正整数，当前: %d", c.Session.Keep)
	}
	return nil
}

var envPattern = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// NeedsSetup 判断是否需要首次引导配置：
// providers 为空、或第一个 provider 的 name 不是 "default"、或 api_key 为空（展开后）。
// 注意：api_key = "${ZLITE_API_KEY}" 且 .env 已设置时，展开后非空，视为已配置。
func (c *Config) NeedsSetup() bool {
	if len(c.Providers) == 0 {
		return true
	}
	p := c.Providers[0]
	if p.Name != "default" {
		return true
	}
	return p.APIKey == ""
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
//   - api_key 追加/更新到配置文件同目录的 .env（ZLITE_API_KEY=...，0600），
//     不覆盖 .env 中已有的其他变量；
//   - config.toml 只更新 [[providers]] 段（保留 agent/shell/tui/session 等其他段），
//     api_key 写 ${ZLITE_API_KEY} 占位，密钥不落盘到配置。
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

	if err := writeDotEnvKey(filepath.Join(filepath.Dir(path), ".env"), "ZLITE_API_KEY", in.APIKey); err != nil {
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
		"api_key":  "${ZLITE_API_KEY}",
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

[[providers]]                    # 一期只取第一个，后期扩展多个
  name = "default"
  type = "openai.chat"           # 厂商.协议：openai.chat | openai.responses
  base_url = "https://api.example.com/v1"
  api_key = "${ZLITE_API_KEY}"   # 放 ~/.zlite/.env: ZLITE_API_KEY=sk-...
  models = ["gpt-4o", "gpt-4o-mini"]   # 可多个，默认使用第一个

[agent]
  mode = "plan"                  # plan（只读）| build（可写）
  auto_approve = false           # 信任模式（跳过危险命令确认）
  max_steps = 16                 # 单轮工具循环上限
  load_agents_md = true          # 自动加载项目根 AGENTS.md 注入系统提示词

[shell]
  confirm_commands = ["rm", "mv", "dd", "mkfs", "sudo", "chmod", "git", "git-push"]

[tui]
  theme = "dark"

[session]
  keep = 20
`

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimPrefix(tpl, "\n")), 0o600); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	return nil
}
