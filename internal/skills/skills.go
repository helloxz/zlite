// Package skills 实现 zlite 的 skills 能力：两级目录的发现与读取。
//
// 兼容 Claude Code 的 SKILL.md 格式（YAML frontmatter: name/description + Markdown body）：
//   - 两级加载：全局 ~/.zlite/skills/ 与项目 <cwd>/.zlite/skills/（项目优先，同名覆盖）
//   - 递归扫描 **/SKILL.md；无 frontmatter 或缺 name/description 的目录跳过
//
// skills 仅作为指令文本注入模型：name+description 列表注入 system prompt，
// 正文由模型经 read_skill 工具按需读取；不改变工具可见性（无 allowed-tools 过滤）。
package skills

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
)

// 来源常量（/skills 列表展示用）。
const (
	SourceGlobal  = "global"
	SourceProject = "project"
)

// 注入数量上限（保守策略，用户决策 2026-08-25）：
// List 返回时全局/项目各自按 name 排序取前 N 个，超出直接丢弃——
// 控制 system prompt 中 skill 清单的规模，避免注意力稀释、效果适得其反。
const (
	MaxGlobalSkills  = 5
	MaxProjectSkills = 10
)

// SkillInfo 是一个已发现 skill 的元信息。
type SkillInfo struct {
	Name        string // frontmatter 的 name
	Description string // frontmatter 的 description（空白已折叠为单行）
	Source      string // SourceGlobal / SourceProject
	Path        string // SKILL.md 的绝对路径
}

// 扫描时跳过的目录（避免无关大目录拖慢遍历）。
var skipDirs = map[string]bool{".git": true, "node_modules": true}

// Manager 管理两级目录的 skills 发现与读取。
//
// 每次 List/Read 前重新扫描目录：对话中新增/修改 skill 立即生效（与 AGENTS.md
// 实时读取语义一致）；skills 目录通常很小，遍历开销可忽略。
// RWMutex 保护内部 map：agent 后台生成线程与 TUI 主循环线程可能并发调用。
type Manager struct {
	globalDir  string
	projectDir string

	mu     sync.RWMutex
	skills map[string]SkillInfo // frontmatter name -> info（同名按项目优先）
	dirs   map[string]SkillInfo // 目录名 -> info（Read 的目录名 fallback）
}

// New 创建 Manager 并执行首次扫描。
// globalDir 为全局 skills 目录（~/.zlite/skills/），projectDir 为项目目录
// （<cwd>/.zlite/skills/）；目录不存在时为空集，不报错。
func New(globalDir, projectDir string) *Manager {
	m := &Manager{globalDir: globalDir, projectDir: projectDir}
	m.refresh()
	return m
}

// List 返回发现的 skills（注入与 /skills 共用数据源）。
// 顺序：项目在前、全局在后（项目与当前任务相关性更高，截断优先丢全局）；
// 各自按 name 排序，并截断到 MaxProjectSkills / MaxGlobalSkills（超出直接丢弃）。
func (m *Manager) List() []SkillInfo {
	m.refresh()
	m.mu.RLock()
	defer m.mu.RUnlock()
	projects := make([]SkillInfo, 0, len(m.skills))
	globals := make([]SkillInfo, 0, len(m.skills))
	for _, s := range m.skills {
		if s.Source == SourceProject {
			projects = append(projects, s)
		} else {
			globals = append(globals, s)
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	sort.Slice(globals, func(i, j int) bool { return globals[i].Name < globals[j].Name })
	if len(projects) > MaxProjectSkills {
		projects = projects[:MaxProjectSkills]
	}
	if len(globals) > MaxGlobalSkills {
		globals = globals[:MaxGlobalSkills]
	}
	out := make([]SkillInfo, 0, len(projects)+len(globals))
	out = append(out, projects...)
	out = append(out, globals...)
	return out
}

// Read 返回指定 skill 的 SKILL.md 全文（含 frontmatter）。
// 先按 frontmatter name 精确匹配，未命中时按目录名匹配（容错：模型可能记目录名）。
// 文件读取失败（如刚被删除）视为不存在。
func (m *Manager) Read(name string) (string, bool) {
	m.refresh()
	m.mu.RLock()
	info, ok := m.skills[name]
	if !ok {
		info, ok = m.dirs[name]
	}
	m.mu.RUnlock()
	if !ok {
		return "", false
	}
	data, err := os.ReadFile(info.Path)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// refresh 重新扫描两级目录并合并（全局先扫，项目后扫覆盖同名）。
func (m *Manager) refresh() {
	skills := map[string]SkillInfo{}
	dirs := map[string]SkillInfo{}
	m.scanDir(m.globalDir, SourceGlobal, skills, dirs)
	m.scanDir(m.projectDir, SourceProject, skills, dirs)
	m.mu.Lock()
	m.skills = skills
	m.dirs = dirs
	m.mu.Unlock()
}

// scanDir 递归扫描 dir 下的 **/SKILL.md。
// 解析失败的文件跳过；同名按覆盖顺序取后者（全局 → 项目）。
func (m *Manager) scanDir(dir, source string, skills, dirs map[string]SkillInfo) {
	if dir == "" {
		return
	}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个目录的权限等错误不影响整体扫描
		}
		if d.IsDir() {
			if p != dir && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "SKILL.md" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		name, desc, ok := parseSKILLMD(data)
		if !ok {
			return nil
		}
		info := SkillInfo{Name: name, Description: desc, Source: source, Path: p}
		skills[name] = info
		dirs[filepath.Base(filepath.Dir(p))] = info
		return nil
	})
}

// parseSKILLMD 解析 SKILL.md：分离 YAML frontmatter 与正文。
//
// frontmatter 以首行 `---` 开始、下一个 `---` 结束；缺 frontmatter、
// YAML 解析失败或 name/description 为空时返回 ok=false（该目录跳过）。
// description 的空白折叠为单行（多行 YAML 块），保证注入列表整洁。
func parseSKILLMD(data []byte) (name, description string, ok bool) {
	content := string(data)
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", false
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return "", "", false
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &meta); err != nil {
		return "", "", false
	}
	name = strings.TrimSpace(meta.Name)
	description = strings.Join(strings.Fields(meta.Description), " ")
	if name == "" || description == "" {
		return "", "", false
	}
	return name, description, true
}
