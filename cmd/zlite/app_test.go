package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/session"
)

// writeValidConfig 在临时 HOME 下写入有效配置。
func writeValidConfig(t *testing.T, home string) {
	t.Helper()
	cfgPath := filepath.Join(home, ".zlite", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `[[providers]]
  name = "default"
  type = "openai-compatible"
  base_url = "http://127.0.0.1:9/v1"
  api_key = "sk-test"
  model = "test-model"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunNoConfigGeneratesTemplate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := run(options{})
	if err == nil || !strings.Contains(err.Error(), "已生成配置模板") {
		t.Fatalf("无配置时应生成模板并提示: %v", err)
	}
	tpl := filepath.Join(home, ".zlite", "config.toml")
	if _, statErr := os.Stat(tpl); statErr != nil {
		t.Errorf("模板未生成: %v", statErr)
	}
}

func TestRunMissingEnvKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, ".zlite", "config.toml")
	os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	os.WriteFile(cfgPath, []byte(`[[providers]]
  name = "default"
  type = "openai-compatible"
  base_url = "http://127.0.0.1:9/v1"
  api_key = "${ZLITE_UNSET_KEY_XYZ}"
  model = "m"
`), 0o600)

	err := run(options{})
	if err == nil || !strings.Contains(err.Error(), "ZLITE_UNSET_KEY_XYZ") {
		t.Fatalf("未设置的环境变量应报错: %v", err)
	}
}

func TestRunBadMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeValidConfig(t, home)

	err := run(options{mode: "autopilot"})
	if err == nil || !strings.Contains(err.Error(), "未知模式") {
		t.Fatalf("非法模式应报错: %v", err)
	}
}

func TestRunContinueNoSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeValidConfig(t, home)

	err := run(options{cont: true})
	if err == nil || !strings.Contains(err.Error(), "没有可继续的会话") {
		t.Fatalf("无会话时 -c 应报错: %v", err)
	}
}

func TestRunListEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeValidConfig(t, home)

	if err := run(options{list: true}); err != nil {
		t.Fatalf("-l 不应报错: %v", err)
	}
}

func TestListSessions(t *testing.T) {
	home := t.TempDir()
	mgr := session.NewManager(filepath.Join(home, "sessions"))
	p := &config.Provider{Name: "default", Model: "test-model"}
	s, err := mgr.Create("/proj", p, config.ModePlan)
	if err != nil {
		t.Fatal(err)
	}
	s.AppendUser("第一条")
	s.AppendUser("第二条")
	s.Close()

	// 捕获 stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err = listSessions(mgr, "/proj")
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)

	if !strings.Contains(string(out), "test-model") || !strings.Contains(string(out), "2") {
		t.Errorf("会话列表输出异常: %q", out)
	}

	// 其他目录无会话
	old = os.Stdout
	r2, w2, _ := os.Pipe()
	os.Stdout = w2
	listSessions(mgr, "/nowhere")
	w2.Close()
	os.Stdout = old
	out2, _ := io.ReadAll(r2)
	if !strings.Contains(string(out2), "没有会话记录") {
		t.Errorf("无会话提示异常: %q", out2)
	}
}
