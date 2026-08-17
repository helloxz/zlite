package agent

import (
	"fmt"
	"runtime"
	"strings"
)

// buildSystemPrompt 组装系统提示词（design.md §4）。
// toolDescs 是当前模式可见工具的描述列表（"name: description"）；
// projectCtx 是项目 AGENTS.md 内容（非空时注入，见 loadProjectContext）；
// skillDescs 是已发现 skills 的描述列表（"name: description (source: ...)"，非空时注入）。
func buildSystemPrompt(cwd string, mode Mode, toolDescs []string, projectCtx string, skillDescs []string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "你是 Zlite，一个运行在终端里的轻量级编程助手。\n\n")

	b.WriteString("环境：\n")
	fmt.Fprintf(&b, "- 操作系统: %s\n", runtime.GOOS)
	fmt.Fprintf(&b, "- 工作目录: %s\n", cwd)
	fmt.Fprintf(&b, "- 当前模式: %s（%s）\n\n", mode, modeDescription(mode))

	b.WriteString("可用工具：\n")
	for _, d := range toolDescs {
		fmt.Fprintf(&b, "- %s\n", d)
	}

	// 项目上下文（AGENTS.md）：存在即注入，AI 遵循项目约定
	if projectCtx != "" {
		b.WriteString("\n## Project Context (AGENTS.md)\n")
		b.WriteString(projectCtx)
		b.WriteString("\n")

		b.WriteString("\n项目规则（Project Context 中的内容优先于通用行为准则）\n")
	}

	// skills 清单（name + description）：正文按需 read_skill 读取，不全文注入
	if len(skillDescs) > 0 {
		b.WriteString("\n## Available Skills\n")
		for _, d := range skillDescs {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		b.WriteString("当任务与某个 skill 匹配时，先调用 read_skill 读取其正文，再遵循其中的指令执行；不匹配则忽略。\n")
	}

	b.WriteString("\n行为准则：\n")
	b.WriteString("- 回答简洁，代码示例用 ```语言 代码块 包裹\n")
	b.WriteString("- 优先使用工具获取事实（读文件、搜索、执行只读命令），不要凭记忆断言文件内容\n")
	b.WriteString("- 任何情况下都不得读取 .env 文件（包含密钥等敏感信息），也不得将其内容写入回复或会话\n")
	b.WriteString("- 当用户询问 zlite 自身相关问题（功能、用法、配置、限制等）时，先用 web_fetch 查阅官方文档 https://note.xiaoz.top/doc/zlite/llms.txt 后再作答，不要凭空猜测\n")
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
