// zlite Bubble Tea 迁移 PoC（M0）
//
// 验证三个风险点：
//   (a) textarea 中文输入（CJK rune 路径：IME commit 与 KeyRunes 同路径）
//   (b) viewport 内 ANSI 头带 + md 着色渲染
//   (c) lipgloss 边框在 CJK 内容下的对齐
//
// 用法：
//   go run . -snapshot        快照模式：渲染结果打到 stdout，不启动 TUI
//   go run .                  交互模式：真实 TUI（textarea + viewport）
//   go run . -simulate-cjk    交互模式 + 启动后自动注入中文（模拟 IME commit）
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	snapshot := flag.Bool("snapshot", false, "render snapshots to stdout and exit")
	simCJK := flag.Bool("simulate-cjk", false, "auto-inject CJK text into the textarea on start")
	flag.Parse()

	if *snapshot {
		runSnapshot()
		return
	}

	p := tea.NewProgram(
		newModel(*simCJK),
		tea.WithAltScreen(),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "poc error:", err)
		os.Exit(1)
	}
}
