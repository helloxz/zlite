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

	"github.com/spf13/viper"
)

// ErrConfigNotFound 表示配置文件不存在。
var ErrConfigNotFound = errors.New("config file not found")

// 模式与类型常量（与 agent 包解耦，避免循环依赖）。
const (
	ModePlan  = "plan"
	ModeBuild = "build"

	// TypeOpenAICompatible 是一期唯一支持的 provider 类型。
	TypeOpenAICompatible = "openai-compatible"
)

// 默认值。
const (
	defaultMode        = ModePlan
	defaultMaxSteps    = 16
	defaultSessionKeep = 20
	defaultTheme       = "dark"
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

// Provider 描述一个模型渠道（OpenAI 兼容端点）。
type Provider struct {
	Name    string `mapstructure:"name"`
	Type    string `mapstructure:"type"`
	BaseURL string `mapstructure:"base_url"`
	APIKey  string `mapstructure:"api_key"` // 支持 ${ENV} 展开
	Model   string `mapstructure:"model"`
}

// AgentCfg 是 agent 行为配置。
type AgentCfg struct {
	Mode        string `mapstructure:"mode"` // plan | build
	AutoApprove bool   `mapstructure:"auto_approve"`
	MaxSteps    int    `mapstructure:"max_steps"`
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
			Mode:        defaultMode,
			AutoApprove: false,
			MaxSteps:    defaultMaxSteps,
		},
		Shell:   ShellCfg{ConfirmCommands: append([]string(nil), defaultConfirmCommands...)},
		TUI:     TUISet{Theme: defaultTheme},
		Session: SessionCfg{Keep: defaultSessionKeep},
	}
}

// DefaultPath 返回默认配置文件路径 ~/.zlite/config.toml。
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录失败: %w", err)
	}
	return filepath.Join(home, ".zlite", "config.toml"), nil
}

// Load 读取配置文件。
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

// DefaultProvider 返回第一个 provider 并校验其合法性（一期单渠道）。
func (c *Config) DefaultProvider() (*Provider, error) {
	if len(c.Providers) == 0 {
		return nil, errors.New("未配置任何 provider，请在配置文件的 [[providers]] 中填写渠道信息")
	}
	p := c.Providers[0] // 副本，避免外部修改内部状态
	if p.Type == "" {
		p.Type = TypeOpenAICompatible // 缺省即 OpenAI 兼容
	}
	if p.Type != TypeOpenAICompatible {
		return nil, fmt.Errorf("providers[0].type 暂不支持: %q（一期仅支持 %q）", p.Type, TypeOpenAICompatible)
	}
	if p.BaseURL == "" {
		return nil, errors.New("providers[0].base_url 未配置")
	}
	if p.Model == "" {
		return nil, errors.New("providers[0].model 未配置")
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
# 修改后需重启 zlite 生效；api_key 支持 ${ENV} 形式引用环境变量，避免密钥落盘。

[[providers]]                    # 一期只取第一个，后期扩展多个
  name = "default"
  type = "openai-compatible"     # OpenAI 兼容自定义端点（一期仅支持此类型）
  base_url = "https://api.example.com/v1"
  api_key = "${ZLITE_API_KEY}"   # 请先 export ZLITE_API_KEY=sk-...
  model = "gpt-4o"

[agent]
  mode = "plan"                  # plan（只读）| build（可写）
  auto_approve = false           # 信任模式（跳过写操作确认）
  max_steps = 16                 # 单轮工具循环上限

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
