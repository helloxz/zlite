package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/llm"
)

func testProvider() *config.Provider {
	return &config.Provider{Name: "default", Type: config.TypeOpenAICompatible, BaseURL: "http://x/v1", Model: "test-model"}
}

func TestCreateStructure(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	cwd := "/data/apps/zlite"

	s, err := m.Create(cwd, testProvider(), config.ModePlan)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 目录: <dir>/<hash>/<id>.jsonl
	rel, err := filepath.Rel(dir, s.Path)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		t.Fatalf("会话路径层级错误: %s", rel)
	}
	if len(parts[0]) != 12 {
		t.Errorf("cwd 哈希目录应为 12 位: %q", parts[0])
	}
	if !strings.HasSuffix(parts[1], ".jsonl") {
		t.Errorf("会话文件名异常: %q", parts[1])
	}

	// 首行是 session 元信息
	recs, err := readAll(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Type != TypeSession {
		t.Fatalf("首行应为 session 元信息: %+v", recs)
	}
	if recs[0].Cwd != cwd || recs[0].Model != "test-model" || recs[0].Mode != "plan" {
		t.Errorf("元信息错误: %+v", recs[0])
	}

	// 不同 cwd 目录不同
	s2, err := m.Create("/other/project", testProvider(), config.ModePlan)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if m.dirFor(cwd) == m.dirFor("/other/project") {
		t.Error("不同 cwd 应生成不同目录")
	}
}

func TestAppendAndContinue(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	cwd := "/data/apps/zlite"

	s, err := m.Create(cwd, testProvider(), config.ModePlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser("你好"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant("我是助手", &llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendToolCall("call_1", "read_file", map[string]any{"path": "a.go"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendToolResult("call_1", "read_file", "package main", false, 12*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMeta("mode_change", "build"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// 继续会话：历史完整、顺序正确
	s2, err := m.Continue(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if s2.ID != s.ID {
		t.Errorf("Continue 应打开同一会话: %q != %q", s2.ID, s.ID)
	}
	if len(s2.History) != 4 {
		t.Fatalf("历史应有 4 条记录（meta 不入 History），得到 %d", len(s2.History))
	}
	if s2.History[0].Content != "你好" || s2.History[2].CallID != "call_1" {
		t.Errorf("历史内容错误: %+v", s2.History)
	}
	if s2.History[1].Usage == nil || s2.History[1].Usage.InputTokens != 10 {
		t.Errorf("usage 恢复错误: %+v", s2.History[1].Usage)
	}
	if s2.Mode != "plan" || s2.Model != "test-model" {
		t.Errorf("会话元信息恢复错误: mode=%q model=%q", s2.Mode, s2.Model)
	}

	// 继续后可追加
	if err := s2.AppendUser("继续"); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	s3, _ := m.Continue(cwd)
	defer s3.Close()
	if len(s3.History) != 5 {
		t.Errorf("追加后历史应为 5 条，得到 %d", len(s3.History))
	}
}

func TestContinueNoSession(t *testing.T) {
	m := NewManager(t.TempDir())
	if _, err := m.Continue("/data/apps/zlite"); err != ErrNoSession {
		t.Fatalf("期望 ErrNoSession，得到: %v", err)
	}
}

func TestContinuePicksLatest(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	cwd := "/data/apps/zlite"

	s1, _ := m.Create(cwd, testProvider(), config.ModePlan)
	s1.AppendUser("第一条")
	s1.Close()
	time.Sleep(10 * time.Millisecond)
	s2, _ := m.Create(cwd, testProvider(), config.ModePlan)
	s2.AppendUser("第二条")
	s2.Close()

	latest, err := m.Continue(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	if latest.ID != s2.ID {
		t.Errorf("Continue 应取最新会话: %q != %q", latest.ID, s2.ID)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	cwd := "/data/apps/zlite"

	s1, _ := m.Create(cwd, testProvider(), config.ModePlan)
	s1.AppendUser("一")
	s1.Close()
	time.Sleep(10 * time.Millisecond)
	s2, _ := m.Create(cwd, testProvider(), config.ModeBuild)
	s2.AppendUser("二")
	s2.AppendUser("三")
	s2.Close()

	infos, err := m.List(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("应有 2 个会话，得到 %d", len(infos))
	}
	// 按时间倒序：最新的在前
	if infos[0].ID != s2.ID {
		t.Errorf("List 应按时间倒序: %+v", infos)
	}
	if infos[0].Messages != 2 || infos[1].Messages != 1 {
		t.Errorf("消息数统计错误: %+v", infos)
	}
	if infos[0].Mode != "build" {
		t.Errorf("mode 统计错误: %+v", infos[0])
	}

	// 其他 cwd 无会话
	other, _ := m.List("/nowhere")
	if len(other) != 0 {
		t.Errorf("其他 cwd 应无会话: %+v", other)
	}
}

func TestToMessagesPairing(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	cwd := "/data/apps/zlite"

	s, err := m.Create(cwd, testProvider(), config.ModePlan)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 模拟一轮带工具的对话
	s.AppendUser("帮我看看")
	s.AppendAssistant("我先读取文件", &llm.Usage{})
	s.AppendToolCall("call_1", "read_file", map[string]any{"path": "a.go"})
	s.AppendToolCall("call_2", "grep", map[string]any{"pattern": "func"})
	s.AppendToolResult("call_1", "read_file", "package main", false, 0)
	s.AppendToolResult("call_2", "grep", "a.go:1:package", false, 0)
	s.AppendAssistant("结论是 package main", nil)

	msgs := s.ToMessages()
	if len(msgs) != 5 {
		t.Fatalf("消息数应为 5（user / assistant+2calls / tool / tool / assistant），得到 %d", len(msgs))
	}
	// [0] user
	if msgs[0].Role != llm.RoleUser || msgs[0].Content != "帮我看看" {
		t.Errorf("user 消息错误: %+v", msgs[0])
	}
	// [1] assistant（带 2 个 tool calls）
	if msgs[1].Role != llm.RoleAssistant || len(msgs[1].ToolCalls) != 2 {
		t.Fatalf("assistant 消息应合并 2 个 tool calls: %+v", msgs[1])
	}
	if msgs[1].ToolCalls[0].ID != "call_1" || msgs[1].ToolCalls[0].Name != "read_file" {
		t.Errorf("tool call 还原错误: %+v", msgs[1].ToolCalls[0])
	}
	// [2][3] tool 结果
	if msgs[2].Role != llm.RoleTool || msgs[2].ToolCallID != "call_1" || msgs[2].Content != "package main" {
		t.Errorf("tool 结果还原错误: %+v", msgs[2])
	}
	if msgs[3].ToolCallID != "call_2" {
		t.Errorf("tool 结果顺序错误: %+v", msgs[3])
	}
	// [4] assistant（无 tool calls）
	if msgs[4].Role != llm.RoleAssistant || len(msgs[4].ToolCalls) != 0 || msgs[4].Content != "结论是 package main" {
		t.Errorf("最终 assistant 消息错误: %+v", msgs[4])
	}
}

func TestAppendToolResultError(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	s, _ := m.Create("/x", testProvider(), config.ModePlan)
	defer s.Close()

	if err := s.AppendToolResult("c1", "run_command", "permission denied", true, 0); err != nil {
		t.Fatal(err)
	}
	msgs := s.ToMessages()
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Content, "error: ") {
		t.Errorf("失败的工具结果应带 error: 前缀: %+v", msgs)
	}
}

func TestFilePermissions(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	s, _ := m.Create("/x", testProvider(), config.ModePlan)
	defer s.Close()

	st, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("会话文件权限应为 0600，得到 %o", perm)
	}
}
