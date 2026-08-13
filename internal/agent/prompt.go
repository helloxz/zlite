package agent

import (
	"fmt"
	"runtime"
	"strings"
)

// buildSystemPrompt 组装系统提示词（design.md §4）。
// toolDescs 是当前模式可见工具的描述列表（"name: description"）。
func buildSystemPrompt(cwd string, mode Mode, toolDescs []string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "你是 zlite，一个运行在终端里的轻量级编程助手。\n\n")

	b.WriteString("环境：\n")
	fmt.Fprintf(&b, "- 操作系统: %s\n", runtime.GOOS)
	fmt.Fprintf(&b, "- 工作目录: %s\n", cwd)
	fmt.Fprintf(&b, "- 当前模式: %s（%s）\n\n", mode, modeDescription(mode))

	b.WriteString("可用工具：\n")
	for _, d := range toolDescs {
		fmt.Fprintf(&b, "- %s\n", d)
	}

	b.WriteString("\n行为准则：\n")
	b.WriteString("- 回答简洁，代码示例用 ```语言 代码块 包裹\n")
	b.WriteString("- 优先使用工具获取事实（读文件、搜索、执行只读命令），不要凭记忆断言文件内容\n")
	if mode == ModePlan {
		b.WriteString("- 当前为只读模式：只做分析与给出方案，不修改任何文件\n")
	} else {
		b.WriteString("- 当前为可写模式：可以直接修改文件、执行命令（破坏性操作会经用户确认）\n")
	}
	b.WriteString("- 修改文件前先 read_file 查看目标内容，替换文本必须精确唯一匹配\n")
	b.WriteString("- 一次只做一件事，工具调用之间等待结果\n")

	return b.String()
}

func modeDescription(m Mode) string {
	if m == ModePlan {
		return "只读模式：只能阅读与搜索，不得修改文件"
	}
	return "可写模式：可修改文件、执行命令"
}
