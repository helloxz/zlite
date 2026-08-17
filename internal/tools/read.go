package tools

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zendev-sh/goai"
)

const (
	defaultReadLimit = 200
	maxMatchLines    = 200
	maxLineShown     = 300 // 匹配行展示的文本上限
)

// 目录遍历时跳过的目录。
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".zlite": true,
}

// isSensitiveEnvFile 判断路径是否指向 .env 敏感文件（.env 及 .env.*，如 .env.local）。
// 这类文件通常含密钥等敏感信息，read_file/grep 一律拒绝访问。
func isSensitiveEnvFile(p string) bool {
	name := filepath.Base(p)
	return name == ".env" || strings.HasPrefix(name, ".env.")
}

// ---- read_file ----

type readFileInput struct {
	Path   string `json:"path" jsonschema:"description=要读取的文件路径（相对工作目录或绝对路径）"`
	Offset int    `json:"offset,omitempty" jsonschema:"description=起始行号（0 基），默认 0"`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=最多读取的行数，默认 200"`
}

func readFileTool(cwd string) Tool {
	return Tool{
		GoAITool: goai.NewTool("read_file", "读取文件内容（带行号）。用于阅读源码、配置文件等。支持 offset/limit 分页读取大文件。",
			func(ctx context.Context, in readFileInput) (string, error) {
				if in.Path == "" {
					return "", fmt.Errorf("path 不能为空")
				}
				p := resolvePath(cwd, in.Path)
				if isSensitiveEnvFile(p) {
					return "", fmt.Errorf("拒绝读取 %s：.env 文件包含密钥等敏感信息，禁止访问", p)
				}
				f, err := os.Open(p)
				if err != nil {
					return "", err
				}
				defer f.Close()

				limit := in.Limit
				if limit <= 0 {
					limit = defaultReadLimit
				}

				var b strings.Builder
				scanner := bufio.NewScanner(f)
				scanner.Buffer(make([]byte, 64*1024), 1024*1024)
				lineNo := 0
				skipped := 0
				for scanner.Scan() {
					if lineNo < in.Offset {
						lineNo++
						skipped++
						continue
					}
					line := scanner.Text()
					// 二进制检测：内容含 NUL 视为二进制文件
					if lineNo == in.Offset && strings.ContainsRune(line, '\x00') {
						return "", fmt.Errorf("%s 看起来是二进制文件，无法以文本读取", p)
					}
					b.WriteString(fmt.Sprintf("%6d→%s\n", lineNo+1, line))
					lineNo++
					if lineNo-in.Offset >= limit {
						break
					}
				}
				if err := scanner.Err(); err != nil {
					return "", err
				}
				if b.Len() == 0 {
					return fmt.Sprintf("（%s：无内容或 offset=%d 超出文件行数 %d）", p, in.Offset, lineNo), nil
				}
				if skipped > 0 {
					return fmt.Sprintf("（已跳过前 %d 行）\n%s", skipped, b.String()), nil
				}
				return b.String(), nil
			}),
		Modes: []Mode{ModePlan, ModeBuild},
	}
}

// ---- grep ----

type grepInput struct {
	Pattern         string `json:"pattern" jsonschema:"description=正则表达式（RE2 语法）"`
	Path            string `json:"path,omitempty" jsonschema:"description=搜索的目录或文件，默认工作目录"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty" jsonschema:"description=是否忽略大小写"`
}

func grepTool(cwd string) Tool {
	return Tool{
		GoAITool: goai.NewTool("grep", "按正则表达式搜索文件内容，返回 file:line:text。默认递归搜索工作目录（跳过 .git/node_modules/.zlite）。",
			func(ctx context.Context, in grepInput) (string, error) {
				if in.Pattern == "" {
					return "", fmt.Errorf("pattern 不能为空")
				}
				re, err := regexp.Compile(in.Pattern)
				if err != nil {
					return "", fmt.Errorf("无效的正则表达式: %w", err)
				}
				if in.CaseInsensitive {
					re = regexp.MustCompile("(?i)" + in.Pattern)
				}

				root := in.Path
				if root == "" {
					root = cwd
				} else {
					root = resolvePath(cwd, root)
				}
				info, err := os.Stat(root)
				if err != nil {
					return "", err
				}

				var b strings.Builder
				count := 0
				if !info.IsDir() {
					count = grepFile(root, re, &b, count)
				} else {
					err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
						if err != nil {
							return nil // 跳过无权限/已删除的路径
						}
						if d.IsDir() {
							if p != root && skipDirs[d.Name()] {
								return filepath.SkipDir
							}
							return nil
						}
						if count >= maxMatchLines {
							return fs.SkipAll
						}
						count = grepFile(p, re, &b, count)
						return nil
					})
					if err != nil && err != fs.SkipAll {
						return "", err
					}
				}
				if b.Len() == 0 {
					return "（无匹配）", nil
				}
				if count >= maxMatchLines {
					b.WriteString(fmt.Sprintf("...[已达 %d 条匹配上限，请缩小范围]\n", maxMatchLines))
				}
				return b.String(), nil
			}),
		Modes: []Mode{ModePlan, ModeBuild},
	}
}

// grepFile 搜索单个文件，把匹配写入 b；返回累计匹配数。
func grepFile(p string, re *regexp.Regexp, b *strings.Builder, count int) int {
	// .env 敏感文件（含密钥）不参与内容搜索，防止匹配行内容泄露
	if isSensitiveEnvFile(p) {
		return count
	}
	f, err := os.Open(p)
	if err != nil {
		return count
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() && count < maxMatchLines {
		line := scanner.Text()
		lineNo++
		if strings.ContainsRune(line, '\x00') {
			return count // 二进制文件
		}
		if re.MatchString(line) {
			shown := line
			if len(shown) > maxLineShown {
				shown = shown[:maxLineShown] + "..."
			}
			fmt.Fprintf(b, "%s:%d:%s\n", p, lineNo, shown)
			count++
		}
	}
	return count
}

// ---- glob ----

type globInput struct {
	Pattern string `json:"pattern" jsonschema:"description=glob 模式，支持 ** 递归匹配任意深度，如 **/*.go、src/**/*_test.go"`
}

func globTool(cwd string) Tool {
	return Tool{
		GoAITool: goai.NewTool("glob", "按文件名模式查找文件（支持 ** 递归），返回相对路径列表。用于发现项目文件结构。",
			func(ctx context.Context, in globInput) (string, error) {
				if in.Pattern == "" {
					return "", fmt.Errorf("pattern 不能为空")
				}
				if strings.Contains(in.Pattern, "**") {
					return globRecursive(cwd, in.Pattern)
				}
				matches, err := filepath.Glob(resolvePath(cwd, in.Pattern))
				if err != nil {
					return "", fmt.Errorf("无效的 glob 模式: %w", err)
				}
				return formatGlobResults(cwd, matches), nil
			}),
		Modes: []Mode{ModePlan, ModeBuild},
	}
}

// globRecursive 处理含 ** 的模式：遍历目录树并对每个文件做段匹配。
func globRecursive(cwd, pattern string) (string, error) {
	patSegs := strings.Split(pattern, "/")
	// 找出固定前缀目录（** 之前的部分），缩小遍历范围
	base := cwd
	for _, seg := range patSegs {
		if seg == "**" || strings.ContainsAny(seg, "*?[") {
			break
		}
		base = path.Join(base, seg)
	}

	var results []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != base && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(results) >= maxMatchLines {
			return fs.SkipAll
		}
		rel := p
		if cwd != "" && strings.HasPrefix(rel, cwd+"/") {
			rel = rel[len(cwd)+1:]
		} else if strings.HasPrefix(rel, "./") {
			rel = rel[2:]
		}
		if matchSegments(patSegs, strings.Split(rel, "/")) {
			results = append(results, rel)
		}
		return nil
	})
	if err != nil && err != fs.SkipAll {
		return "", err
	}
	return formatGlobResults(cwd, results), nil
}

// matchSegments 逐段匹配 glob 模式（支持 **）。
func matchSegments(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		// ** 匹配 0..n 段
		for i := 0; i <= len(name); i++ {
			if matchSegments(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], name[1:])
}

func formatGlobResults(cwd string, matches []string) string {
	if len(matches) == 0 {
		return "（无匹配文件）"
	}
	sort.Strings(matches)
	var b strings.Builder
	for _, m := range matches {
		if cwd != "" && strings.HasPrefix(m, cwd+"/") {
			m = m[len(cwd)+1:]
		}
		fmt.Fprintln(&b, m)
	}
	if len(matches) >= maxMatchLines {
		b.WriteString(fmt.Sprintf("...[已达 %d 条上限，请缩小范围]\n", maxMatchLines))
	}
	return b.String()
}
