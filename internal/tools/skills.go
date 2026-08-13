package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/zendev-sh/goai"
)

// SkillReader 是 read_skill 工具依赖的 skills 查询能力
// （*skills.Manager 实现；接口定义在本包，避免 tools 强依赖 skills 包）。
type SkillReader interface {
	Read(name string) (string, bool)
}

// readSkillInput 是 read_skill 工具的输入。
type readSkillInput struct {
	Name string `json:"name" jsonschema:"description=skill 名称（/skills 可查看列表；frontmatter name 或目录名均可）"`
}

// ReadSkillTool 构造 read_skill 工具：按 name 读取 SKILL.md 全文（含 frontmatter），
// 供模型按需加载技能指令。全模式（plan/build）可用，无需确认。
// reader 为 nil 时工具不可用（执行时报错，正常组装不会发生）。
func ReadSkillTool(reader SkillReader) Tool {
	return Tool{
		GoAITool: goai.NewTool("read_skill", "读取指定 skill 的完整内容（SKILL.md，含 frontmatter 与正文）。技能清单已注入系统提示词；当任务与某个 skill 匹配时，调用本工具获取其指令并遵循。",
			func(ctx context.Context, in readSkillInput) (string, error) {
				if reader == nil {
					return "", fmt.Errorf("skills 未启用")
				}
				name := strings.TrimSpace(in.Name)
				if name == "" {
					return "", fmt.Errorf("name 不能为空")
				}
				content, ok := reader.Read(name)
				if !ok {
					return "", fmt.Errorf("skill %q 不存在（/skills 可查看已发现的 skills）", name)
				}
				return content, nil
			}),
		Modes: []Mode{ModePlan, ModeBuild},
	}
}
