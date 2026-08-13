package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeToml(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	return path
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("期望 ErrConfigNotFound，得到: %v", err)
	}
}

func TestLoadFullConfig(t *testing.T) {
	t.Setenv("TEST_ZLITE_KEY", "sk-test-123")
	path := writeToml(t, `
[[providers]]
  name = "primary"
  type = "openai-compatible"
  base_url = "https://llm.example.com/v1"
  api_key = "${TEST_ZLITE_KEY}"
  model = "test-model"

[[providers]]
  name = "backup"
  type = "openai-compatible"
  base_url = "https://llm2.example.com/v1"
  api_key = "plain-key"
  model = "backup-model"

[agent]
  mode = "build"
  auto_approve = true
  max_steps = 8

[shell]
  confirm_commands = ["rm", "git"]

[tui]
  theme = "light"

[session]
  keep = 5
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if len(cfg.Providers) != 2 {
		t.Fatalf("期望 2 个 provider，得到 %d", len(cfg.Providers))
	}
	if cfg.Providers[0].Name != "primary" || cfg.Providers[0].APIKey != "sk-test-123" || cfg.Providers[0].Model != "test-model" {
		t.Errorf("providers[0] 解析错误: %+v", cfg.Providers[0])
	}
	if cfg.Providers[1].APIKey != "plain-key" {
		t.Errorf("明文 api_key 应原样保留，得到: %q", cfg.Providers[1].APIKey)
	}
	if cfg.Agent.Mode != "build" || !cfg.Agent.AutoApprove || cfg.Agent.MaxSteps != 8 {
		t.Errorf("agent 解析错误: %+v", cfg.Agent)
	}
	if !reflect.DeepEqual(cfg.Shell.ConfirmCommands, []string{"rm", "git"}) {
		t.Errorf("confirm_commands 解析错误: %v", cfg.Shell.ConfirmCommands)
	}
	if cfg.TUI.Theme != "light" || cfg.Session.Keep != 5 {
		t.Errorf("tui/session 解析错误: theme=%q keep=%d", cfg.TUI.Theme, cfg.Session.Keep)
	}

	p, err := cfg.DefaultProvider()
	if err != nil {
		t.Fatalf("DefaultProvider 失败: %v", err)
	}
	if p.Name != "primary" {
		t.Errorf("DefaultProvider 应返回第一个 provider，得到 %q", p.Name)
	}
}

func TestLoadPartialConfigKeepsDefaults(t *testing.T) {
	path := writeToml(t, `
[[providers]]
  name = "default"
  type = "openai-compatible"
  base_url = "http://localhost:8080/v1"
  api_key = "local"
  model = "local-model"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if cfg.Agent.Mode != ModePlan || cfg.Agent.AutoApprove || cfg.Agent.MaxSteps != 16 {
		t.Errorf("agent 默认值错误: %+v", cfg.Agent)
	}
	if !reflect.DeepEqual(cfg.Shell.ConfirmCommands, defaultConfirmCommands) {
		t.Errorf("confirm_commands 默认值错误: %v", cfg.Shell.ConfirmCommands)
	}
	if cfg.TUI.Theme != defaultTheme || cfg.Session.Keep != defaultSessionKeep {
		t.Errorf("tui/session 默认值错误: theme=%q keep=%d", cfg.TUI.Theme, cfg.Session.Keep)
	}
}

func TestLoadEnvUnset(t *testing.T) {
	// 注意: 不设置 TEST_ZLITE_KEY
	path := writeToml(t, `
[[providers]]
  name = "default"
  type = "openai-compatible"
  base_url = "https://llm.example.com/v1"
  api_key = "${TEST_ZLITE_KEY}"
  model = "test-model"
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "TEST_ZLITE_KEY") {
		t.Fatalf("期望环境变量未设置错误，得到: %v", err)
	}
}

func TestLoadInvalidMode(t *testing.T) {
	path := writeToml(t, `
[[providers]]
  name = "default"
  type = "openai-compatible"
  base_url = "https://llm.example.com/v1"
  api_key = "k"
  model = "m"

[agent]
  mode = "autopilot"
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "agent.mode") {
		t.Fatalf("期望 mode 校验错误，得到: %v", err)
	}
}

func TestDefaultProviderErrors(t *testing.T) {
	// 无 provider
	cfg := DefaultConfig()
	if _, err := cfg.DefaultProvider(); err == nil {
		t.Fatal("空 providers 应报错")
	}

	// 不支持的 type
	cfg.Providers = []Provider{{Name: "x", Type: "anthropic", BaseURL: "http://x/v1", Model: "m"}}
	if _, err := cfg.DefaultProvider(); err == nil || !strings.Contains(err.Error(), "暂不支持") {
		t.Fatalf("期望 type 校验错误，得到: %v", err)
	}

	// 缺 base_url
	cfg.Providers = []Provider{{Name: "x", Type: TypeOpenAICompatible, Model: "m"}}
	if _, err := cfg.DefaultProvider(); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("期望 base_url 校验错误，得到: %v", err)
	}

	// 缺 model
	cfg.Providers = []Provider{{Name: "x", Type: TypeOpenAICompatible, BaseURL: "http://x/v1"}}
	if _, err := cfg.DefaultProvider(); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("期望 model 校验错误，得到: %v", err)
	}

	// 缺省 type 自动补为 openai-compatible
	cfg.Providers = []Provider{{Name: "x", BaseURL: "http://x/v1", Model: "m"}}
	p, err := cfg.DefaultProvider()
	if err != nil {
		t.Fatalf("缺省 type 应合法: %v", err)
	}
	if p.Type != TypeOpenAICompatible {
		t.Errorf("缺省 type 应为 %q，得到 %q", TypeOpenAICompatible, p.Type)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("ZLITE_EXPAND_TEST", "expanded")
	cases := []struct {
		in   string
		want string
	}{
		{"${ZLITE_EXPAND_TEST}", "expanded"},
		{"plain-key", "plain-key"},
		{"${ZLITE_EXPAND_TEST}-suffix", "${ZLITE_EXPAND_TEST}-suffix"}, // 拼接形式一期不支持
		{"", ""},
	}
	for _, c := range cases {
		got, err := expandEnv(c.in)
		if err != nil {
			t.Errorf("expandEnv(%q) 意外错误: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("expandEnv(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	if _, err := expandEnv("${ZLITE_EXPAND_UNSET_VAR}"); err == nil {
		t.Error("未设置的环境变量应报错")
	}
}

func TestWriteTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteTemplate(path); err != nil {
		t.Fatalf("WriteTemplate 失败: %v", err)
	}

	// 重复写入应报错（不覆盖）
	if err := WriteTemplate(path); err == nil {
		t.Fatal("重复 WriteTemplate 应报错")
	}

	// 模板应能被 Load 读回（配合环境变量）
	t.Setenv("ZLITE_API_KEY", "sk-template")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("模板 Load 失败: %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Type != TypeOpenAICompatible {
		t.Errorf("模板 providers 解析错误: %+v", cfg.Providers)
	}
	if cfg.Providers[0].APIKey != "sk-template" {
		t.Errorf("模板 api_key 展开错误: %q", cfg.Providers[0].APIKey)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("HOME", "/tmp/fakehome")
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath 失败: %v", err)
	}
	want := "/tmp/fakehome/.zlite/config.toml"
	if got != want {
		t.Errorf("DefaultPath = %q，期望 %q", got, want)
	}
}
