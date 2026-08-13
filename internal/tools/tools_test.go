package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zendev-sh/goai"
)

// ---- 测试辅助 ----

func toolByName(t *testing.T, r *Registry, name string) goai.Tool {
	t.Helper()
	for _, tl := range r.ForMode(ModePlan) {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("工具 %s 未注册", name)
	return goai.Tool{}
}

func callTool(t *testing.T, tl goai.Tool, input any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("序列化输入失败: %v", err)
	}
	return tl.Execute(context.Background(), raw)
}

// newTestTree 创建测试目录结构，返回 cwd。
func newTestTree(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(cwd, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n\nfunc hello() {\n\tprintln(\"Hi\")\n}\n")
	write("sub/deep/x.txt", "some text\n")
	write(".git/config", "secret=should-be-skipped\n")
	write("node_modules/pkg/index.js", "module.exports = 1\n")
	return cwd
}

// ---- registry ----

func TestForMode(t *testing.T) {
	r := New(t.TempDir(), []string{"rm", "mv"})
	plan := r.ForMode(ModePlan)
	build := r.ForMode(ModeBuild)
	if len(plan) != 5 {
		t.Errorf("plan 模式应暴露 5 个工具（4 只读 + run_command 只读版），得到 %d: %v", len(plan), names(plan))
	}
	if len(build) != 8 {
		t.Errorf("build 模式应暴露 8 个工具（4 只读 + run_command 全量版 + write/edit/delete），得到 %d: %v", len(build), names(build))
	}
	if need, _ := r.NeedApproveFor("read_file", map[string]any{}); need {
		t.Error("只读工具不应需要确认")
	}
	// build 模式下危险命令需要确认
	if need, _ := r.NeedApproveFor("run_command", map[string]any{"command": "rm -rf x"}); !need {
		t.Error("危险命令应触发确认")
	}
	if need, _ := r.NeedApproveFor("run_command", map[string]any{"command": "ls -la"}); need {
		t.Error("普通命令不应触发确认")
	}
	// 写工具直接执行（无需确认，用户决策 D3 修订）
	for _, name := range []string{"write_file", "edit_file", "delete"} {
		if need, _ := r.NeedApproveFor(name, map[string]any{}); need {
			t.Errorf("%s 不应需要确认", name)
		}
	}
}

func names(ts []goai.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

// ---- read_file ----

func TestReadFile(t *testing.T) {
	cwd := newTestTree(t)
	r := New(cwd, nil)
	tl := toolByName(t, r, "read_file")

	out, err := callTool(t, tl, map[string]any{"path": "main.go"})
	if err != nil {
		t.Fatalf("read_file 失败: %v", err)
	}
	if !strings.Contains(out, "func hello()") || !strings.Contains(out, "package main") {
		t.Errorf("read_file 输出异常: %q", out)
	}

	// offset/limit 分页
	out, err = callTool(t, tl, map[string]any{"path": "main.go", "offset": 1, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "package main") || !strings.Contains(out, "func hello()") {
		t.Errorf("offset 分页异常: %q", out)
	}

	// 越界安全
	out, err = callTool(t, tl, map[string]any{"path": "main.go", "offset": 100})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "无内容") {
		t.Errorf("越界应提示无内容: %q", out)
	}

	// 不存在的文件
	if _, err := callTool(t, tl, map[string]any{"path": "nope.go"}); err == nil {
		t.Error("不存在的文件应报错")
	}

	// 二进制检测
	bin := filepath.Join(cwd, "bin.dat")
	os.WriteFile(bin, []byte{0x00, 0x01, 0x02, 0xff}, 0o644)
	if _, err := callTool(t, tl, map[string]any{"path": "bin.dat"}); err == nil || !strings.Contains(err.Error(), "二进制") {
		t.Errorf("二进制文件应报错: %v", err)
	}
}

// ---- grep ----

func TestGrep(t *testing.T) {
	cwd := newTestTree(t)
	r := New(cwd, nil)
	tl := toolByName(t, r, "grep")

	out, err := callTool(t, tl, map[string]any{"pattern": "func hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go:3:") {
		t.Errorf("grep 应匹配 main.go 第 3 行: %q", out)
	}
	// .git 与 node_modules 应被跳过
	if strings.Contains(out, "should-be-skipped") || strings.Contains(out, "index.js") {
		t.Errorf("grep 不应进入跳过目录: %q", out)
	}

	// 大小写
	out, err = callTool(t, tl, map[string]any{"pattern": "FUNC HELLO", "case_insensitive": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go:3:") {
		t.Errorf("忽略大小写未生效: %q", out)
	}

	// 无匹配
	out, err = callTool(t, tl, map[string]any{"pattern": "zzz_nothing"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "无匹配") {
		t.Errorf("无匹配提示异常: %q", out)
	}

	// 指定文件
	out, err = callTool(t, tl, map[string]any{"pattern": "some", "path": "sub/deep/x.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sub/deep/x.txt:1:") {
		t.Errorf("指定文件搜索异常: %q", out)
	}
}

// ---- glob ----

func TestGlob(t *testing.T) {
	cwd := newTestTree(t)
	r := New(cwd, nil)
	tl := toolByName(t, r, "glob")

	out, err := callTool(t, tl, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "main.go" {
		t.Errorf("**/*.go 应只匹配 main.go（相对路径）: %q", out)
	}

	out, err = callTool(t, tl, map[string]any{"pattern": "sub/**"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "sub/deep/x.txt" {
		t.Errorf("sub/** 匹配异常: %q", out)
	}

	// 跳过目录中的文件不应出现
	out, err = callTool(t, tl, map[string]any{"pattern": "**/*.js"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "index.js") {
		t.Errorf("glob 不应进入 node_modules: %q", out)
	}

	// 无匹配
	out, err = callTool(t, tl, map[string]any{"pattern": "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "无匹配") {
		t.Errorf("无匹配提示异常: %q", out)
	}
}

// ---- web_fetch ----

func TestWebFetch(t *testing.T) {
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<html><head><script>var x=1;</script><style>.a{}</style></head><body><h1>标题</h1><p>你好 &amp; 世界</p><!-- comment --></body></html>")
		case "/data":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true,"n":42}`)
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, strings.Repeat("a", 1024*1024+500))
		case "/missing":
			http.NotFound(w, r)
		}
	}))
	defer htmlSrv.Close()

	r := New(t.TempDir(), nil)
	tl := toolByName(t, r, "web_fetch")

	out, err := callTool(t, tl, map[string]any{"url": htmlSrv.URL + "/page"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "标题") || !strings.Contains(out, "你好 & 世界") {
		t.Errorf("HTML 转文本异常: %q", out)
	}
	if strings.Contains(out, "script") || strings.Contains(out, "comment") {
		t.Errorf("script/注释未剥离: %q", out)
	}

	// 非 HTML 原样返回
	out, err = callTool(t, tl, map[string]any{"url": htmlSrv.URL + "/data"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"ok":true`) {
		t.Errorf("JSON 应原样返回: %q", out)
	}

	// 404 报错
	if _, err := callTool(t, tl, map[string]any{"url": htmlSrv.URL + "/missing"}); err == nil {
		t.Error("404 应报错")
	}

	// 超大响应截断
	out, err = callTool(t, tl, map[string]any{"url": htmlSrv.URL + "/big"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已截断") {
		t.Error("超 1MB 响应应提示截断")
	}

	// 非法协议
	if _, err := callTool(t, tl, map[string]any{"url": "ftp://x"}); err == nil {
		t.Error("非 http/https 应报错")
	}
}

// ---- run_command（plan 白名单）----

func TestValidateReadOnlyCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		ok   bool
		want string
	}{
		{"ls -la", true, ""},
		{"cat main.go", true, ""},
		{"pwd", true, ""},
		{"git status", true, ""},
		{"git diff --stat", true, ""},
		{"echo hello", true, ""},
		{"rm main.go", false, "不允许命令"},
		{"mv a b", false, "不允许命令"},
		{"cat a > b", false, "shell 元字符"},
		{"ls | grep go", false, "shell 元字符"},
		{"ls; rm x", false, "shell 元字符"},
		{"git push", false, "git 只读子命令"},
		{"git reset --hard", false, "git 只读子命令"},
		{"$(rm -rf /)", false, "shell 元字符"},
		{"", false, "空命令"},
	}
	for _, c := range cases {
		err := validateReadOnlyCommand(c.cmd)
		if c.ok && err != nil {
			t.Errorf("validateReadOnlyCommand(%q) 应通过，得到: %v", c.cmd, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("validateReadOnlyCommand(%q) 应拒绝（含 %q），得到: %v", c.cmd, c.want, err)
		}
	}
}

func TestRunCommandExec(t *testing.T) {
	cwd := newTestTree(t)
	r := New(cwd, nil)
	tl := toolByName(t, r, "run_command")

	out, err := callTool(t, tl, map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != cwd {
		t.Errorf("pwd 应返回 cwd: %q != %q", strings.TrimSpace(out), cwd)
	}

	out, err = callTool(t, tl, map[string]any{"command": "cat main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "func hello") {
		t.Errorf("cat 输出异常: %q", out)
	}

	if _, err := callTool(t, tl, map[string]any{"command": "rm main.go"}); err == nil {
		t.Error("plan 模式应拒绝 rm")
	}

	// 命令不存在
	if _, err := callTool(t, tl, map[string]any{"command": "definitely-not-a-cmd-xyz"}); err == nil {
		t.Error("不存在的命令应报错")
	}
}

// ---- truncate ----

func TestTruncate(t *testing.T) {
	big := strings.Repeat("a", MaxOutputBytes+100)
	out := truncate(big)
	if len(out) > MaxOutputBytes+50 {
		t.Errorf("截断后仍过大: %d", len(out))
	}
	if !strings.Contains(out, "[truncated]") {
		t.Error("截断应有标记")
	}
	if !strings.HasSuffix(out, "aaaaa") {
		t.Error("截断应保留尾部")
	}
	small := "hi"
	if truncate(small) != small {
		t.Error("小输出不应被截断")
	}
}

// ---- build 模式：写工具与全量 shell ----

func buildToolByName(t *testing.T, r *Registry, name string) goai.Tool {
	t.Helper()
	for _, tl := range r.ForMode(ModeBuild) {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("build 工具 %s 未注册", name)
	return goai.Tool{}
}

func TestWriteFile(t *testing.T) {
	cwd := t.TempDir()
	r := New(cwd, nil)
	tl := buildToolByName(t, r, "write_file")

	out, err := callTool(t, tl, map[string]any{"path": "a/b.txt", "content": "hello"})
	if err != nil {
		t.Fatalf("write_file 失败: %v", err)
	}
	if !strings.Contains(out, "Wrote 5 bytes") {
		t.Errorf("输出异常: %q", out)
	}
	data, err := os.ReadFile(filepath.Join(cwd, "a", "b.txt"))
	if err != nil || string(data) != "hello" {
		t.Errorf("文件内容错误: %q %v", data, err)
	}
}

func TestEditFile(t *testing.T) {
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(cwd, "f.txt"), []byte("aaa\nbbb\naaa\n"), 0o644)
	r := New(cwd, nil)
	tl := buildToolByName(t, r, "edit_file")

	// 唯一匹配
	out, err := callTool(t, tl, map[string]any{"path": "f.txt", "old_string": "bbb", "new_string": "XXX"})
	if err != nil {
		t.Fatalf("edit_file 失败: %v", err)
	}
	if !strings.Contains(out, "Replaced 1 occurrence") {
		t.Errorf("输出异常: %q", out)
	}
	data, _ := os.ReadFile(filepath.Join(cwd, "f.txt"))
	if string(data) != "aaa\nXXX\naaa\n" {
		t.Errorf("替换结果错误: %q", data)
	}

	// 多重匹配报错
	_, err = callTool(t, tl, map[string]any{"path": "f.txt", "old_string": "aaa", "new_string": "Y"})
	if err == nil || !strings.Contains(err.Error(), "2 次") {
		t.Errorf("多重匹配应报错: %v", err)
	}

	// 无匹配报错
	_, err = callTool(t, tl, map[string]any{"path": "f.txt", "old_string": "zzz", "new_string": "Y"})
	if err == nil || !strings.Contains(err.Error(), "未在") {
		t.Errorf("无匹配应报错: %v", err)
	}
}

func TestDelete(t *testing.T) {
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(cwd, "d.txt"), []byte("x"), 0o644)
	r := New(cwd, nil)
	tl := buildToolByName(t, r, "delete")

	out, err := callTool(t, tl, map[string]any{"path": "d.txt"})
	if err != nil {
		t.Fatalf("delete 失败: %v", err)
	}
	if !strings.Contains(out, "Deleted") {
		t.Errorf("输出异常: %q", out)
	}
	if _, err := os.Stat(filepath.Join(cwd, "d.txt")); !os.IsNotExist(err) {
		t.Error("文件应已删除")
	}

	// 目录应拒绝
	os.MkdirAll(filepath.Join(cwd, "dir"), 0o755)
	if _, err := callTool(t, tl, map[string]any{"path": "dir"}); err == nil || !strings.Contains(err.Error(), "目录") {
		t.Errorf("删除目录应报错: %v", err)
	}
}

func TestRunCommandBuildMode(t *testing.T) {
	cwd := t.TempDir()
	r := New(cwd, []string{"rm", "mv", "dd", "mkfs", "sudo", "chmod", "git"})
	tl := buildToolByName(t, r, "run_command")

	// build 模式可执行写操作（无需确认）
	out, err := callTool(t, tl, map[string]any{"command": "touch new.txt"})
	if err != nil {
		t.Fatalf("build 模式 touch 应成功: %v", err)
	}
	_ = out
	if _, err := os.Stat(filepath.Join(cwd, "new.txt")); err != nil {
		t.Error("touch 应创建文件")
	}

	// 危险命令触发确认（NeedApprove）
	need, summary := r.NeedApproveFor("run_command", map[string]any{"command": "rm new.txt"})
	if !need || !strings.Contains(summary, "dangerous") {
		t.Errorf("rm 应触发确认: %v %q", need, summary)
	}
	need, _ = r.NeedApproveFor("run_command", map[string]any{"command": "mv a b"})
	if !need {
		t.Error("mv 应触发确认")
	}
	need, _ = r.NeedApproveFor("run_command", map[string]any{"command": "git push"})
	if !need {
		t.Error("git push 应触发确认")
	}
	need, _ = r.NeedApproveFor("run_command", map[string]any{"command": "git status"})
	if need {
		t.Error("git status 不应触发确认")
	}
	need, _ = r.NeedApproveFor("run_command", map[string]any{"command": "ls -la"})
	if need {
		t.Error("ls 不应触发确认")
	}
	need, _ = r.NeedApproveFor("run_command", map[string]any{"command": "rm -rf /"})
	if !need {
		t.Error("rm -rf / 应触发确认")
	}

	// plan 模式拒绝写命令
	planTl := toolByName(t, r, "run_command")
	if _, err := callTool(t, planTl, map[string]any{"command": "touch x.txt"}); err == nil {
		t.Error("plan 模式应拒绝 touch")
	}
}
