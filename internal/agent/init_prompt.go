package agent

import (
	"fmt"
	"strings"
)

// initSystemPrompt 生成 /init 任务的系统指令：分析项目并生成/更新 AGENTS.md。
// 方法论参考 Reasonix 内置 init skill：先查已有文档（改进而非覆盖）、
// 探索项目、验证命令、输出固定结构、保持精简。
func initSystemPrompt(cwd string, mode Mode) string {
	var b strings.Builder

	b.WriteString("你是 zlite，正在执行项目初始化任务（/init）：分析项目并生成 AGENTS.md（项目概述文件）。\n\n")
	fmt.Fprintf(&b, "环境：\n- 工作目录: %s\n- 当前模式: %s（%s）\n\n", cwd, mode, modeDescription(mode))

	b.WriteString("操作步骤：\n")
	b.WriteString("1. 先检查工作目录是否已存在 AGENTS.md：\n")
	b.WriteString("   - 存在：用 read_file 读取，在保留原有有效内容的基础上改进（修正过时信息、补充缺失项），写回同一文件，不要整体覆盖或另建新文件\n")
	b.WriteString("   - 不存在：按以下步骤全新生成\n")
	b.WriteString("2. 探索项目（用工具获取事实，不要凭记忆）：\n")
	b.WriteString("   - 项目形态：glob 查看目录结构；读取 manifest（go.mod/package.json/pyproject.toml/Cargo.toml 等）与 README\n")
	b.WriteString("   - 构建/测试/运行命令：从 manifest 与脚本推导，并用 run_command 验证确切命令名（不要猜测）\n")
	b.WriteString("   - 架构：主要包/模块及其职责、入口点\n")
	b.WriteString("   - 约定：从代表性源码推断（格式化、命名、错误处理、测试模式），不要凭空假设\n")
	b.WriteString("3. 产出 AGENTS.md：\n")
	if mode == ModePlan {
		b.WriteString("   - 当前为 plan 模式：将 AGENTS.md 完整内容直接输出到回复中（代码块内），不写文件\n")
		b.WriteString("   - 提示用户：切到 build 模式后重新执行 /init 即可写入文件\n")
	} else {
		b.WriteString("   - 当前为 build 模式：用 write_file 将内容写入 AGENTS.md（已有则用 edit_file 改进）\n")
	}
	b.WriteString("   结构（保持精简，每行都要有信息量，不要太啰嗦）：\n")
	b.WriteString("   - 标题 + 项目一句话描述\n")
	b.WriteString("   - ## Project：项目是什么、技术栈、入口位置\n")
	b.WriteString("   - ## Commands：确切的构建/测试/运行/检查命令\n")
	b.WriteString("   - ## Architecture：核心框架与技术栈、3-7 个关键模块及其职责、整体设计架构说明（模块如何协作、数据流）\n")
	b.WriteString("   - ## Conventions：agent 必须遵守的规则（风格、模式、做/不做）\n")
	b.WriteString("   - ## Notes：留空小节便于日后补充\n")
	b.WriteString("4. 收尾：用一两句话总结生成/改进的内容，并提示用户审阅修改。\n\n")

	b.WriteString("规则：\n")
	b.WriteString("- 命令与路径必须与实际文件一致（错误的构建命令比没有更糟）\n")
	b.WriteString("- 不要编造代码中不存在的约定\n")
	b.WriteString("- 内容精简（它会在每次会话加载进上下文，每行都消耗 token）\n")
	b.WriteString("- 绝不包含密钥或敏感信息\n")
	b.WriteString("- 项目约定（写入 ## Conventions，作为长期编码规范）：\n")
	b.WriteString("  - 核心代码的关键位置写中文注释（函数意图、复杂逻辑说明）\n")
	b.WriteString("  - 编写代码时适当考虑性能优化（避免明显低效的写法）\n")
	b.WriteString("  - 所有用户可见文案（后端响应内容、提示、TUI 界面）一律用英文\n")

	return b.String()
}
