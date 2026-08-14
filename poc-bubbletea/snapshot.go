package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// runSnapshot 快照模式：不启动 TUI，验证 (b) viewport ANSI 渲染与
// (c) lipgloss 边框 CJK 对齐（程序内断言），结果打到 stdout。
func runSnapshot() {
	fmt.Println("===== (b) 头带 + md 着色（无边框，直接作为 viewport 内容） =====")
	const w = 60
	content := sampleChat(w)
	// 宽度自检：每行显示宽度应 <= w（除空行）
	for i, ln := range strings.Split(content, "\n") {
		lw := ansi.StringWidth(ln)
		flag := "ok"
		if lw > w {
			flag = "OVERFLOW"
		}
		if lw > 0 {
			fmt.Printf("  line %2d width=%2d [%s] %s\n", i, lw, flag, truncateVisible(ln, 50))
		}
	}

	fmt.Println()
	fmt.Println("===== (b) viewport 渲染结果（宽 60 高 12，ANSII 保留检查） =====")
	vp := viewport.New(60, 12)
	vp.SetContent(content)
	vp.GotoBottom()
	rendered := vp.View()
	fmt.Println(rendered)
	hasSGR := strings.Contains(rendered, "\x1b[")
	fmt.Printf("  -> SGR 序列保留: %v\n", hasSGR)

	fmt.Println()
	fmt.Println("===== (c) lipgloss 边框 + CJK 对齐断言 =====")
	checkBorder("RoundedBorder", lipgloss.RoundedBorder(), 24, "你好，世界 Hello 对齐测试")
	checkBorder("NormalBorder(ASCII)", lipgloss.NormalBorder(), 24, "你好，世界 Hello 对齐测试")
	checkBorder("RoundedBorder 状态栏", lipgloss.RoundedBorder(), 40, "[plan] poc-model | thinking: auto | ready")

	fmt.Println()
	fmt.Println("===== (c) CJK 宽度计算抽查 =====")
	for _, s := range []string{
		"你好",
		"Hello",
		"你好，世界！",
		"abc 中文 def",
		"`inline code` 中文",
	} {
		fmt.Printf("  StringWidth(%q) = %d\n", s, ansi.StringWidth(s))
	}
	fmt.Println("  (预期：汉字/全角各 2，ASCII 各 1)")
}

func checkBorder(name string, border lipgloss.Border, width int, content string) {
	style := lipgloss.NewStyle().Border(border, true, true, true, true).Width(width).Padding(0, 1)
	out := style.Render(content)
	expected := width + 2 // lipgloss: Width 含 padding、不含 border，border 左右各 1
	ok := true
	for i, ln := range strings.Split(out, "\n") {
		lw := ansi.StringWidth(ln)
		if lw != expected {
			ok = false
			fmt.Printf("  line %2d width=%2d expected=%2d  %q\n", i, lw, expected, ln)
		}
	}
	// 打印盒子便于目测
	for _, ln := range strings.Split(out, "\n") {
		fmt.Printf("  |%s|\n", ln)
	}
	verdict := "PASS"
	if !ok {
		verdict = "FAIL"
	}
	fmt.Printf("  -> %s: 每行宽度均 = %d\n\n", verdict, expected)
}

func truncateVisible(s string, n int) string {
	if ansi.StringWidth(s) <= n {
		return s
	}
	return s[:n] + "…"
}
