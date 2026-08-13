package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zendev-sh/goai"
)

// 写工具（write_file / edit_file / delete）仅在 build 模式注入，
// 按用户决策（D3 修订）：build 模式下写操作直接执行，无需确认。

// ---- write_file ----

type writeFileInput struct {
	Path    string `json:"path" jsonschema:"description=要写入的文件路径（相对工作目录或绝对路径）"`
	Content string `json:"content" jsonschema:"description=完整文件内容（覆盖写入）"`
}

func writeFileTool(cwd string) Tool {
	return Tool{
		GoAITool: goai.NewTool("write_file", "创建新文件或整体覆写已有文件（自动创建父目录）。内容会完全替换文件现有内容，写前请先用 read_file 查看。",
			func(ctx context.Context, in writeFileInput) (string, error) {
				if in.Path == "" {
					return "", fmt.Errorf("path 不能为空")
				}
				p := resolvePath(cwd, in.Path)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return "", fmt.Errorf("创建父目录失败: %w", err)
				}
				if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
					return "", err
				}
				return fmt.Sprintf("Wrote %d bytes to %s", len(in.Content), in.Path), nil
			}),
		Modes: []Mode{ModeBuild},
	}
}

// ---- edit_file ----

type editFileInput struct {
	Path      string `json:"path" jsonschema:"description=要修改的文件路径"`
	OldString string `json:"old_string" jsonschema:"description=要替换的原文（必须在文件中恰好出现一次）"`
	NewString string `json:"new_string" jsonschema:"description=替换后的新文本"`
}

func editFileTool(cwd string) Tool {
	return Tool{
		GoAITool: goai.NewTool("edit_file", "精确替换文件中的文本片段。old_string 必须在文件中恰好出现一次（0 次或多次会报错，请先 read_file 获取准确上下文）。用于修改已有文件。",
			func(ctx context.Context, in editFileInput) (string, error) {
				if in.Path == "" || in.OldString == "" {
					return "", fmt.Errorf("path 与 old_string 不能为空")
				}
				p := resolvePath(cwd, in.Path)
				data, err := os.ReadFile(p)
				if err != nil {
					return "", err
				}
				content := string(data)
				count := strings.Count(content, in.OldString)
				if count == 0 {
					return "", fmt.Errorf("old_string 未在 %s 中找到（请先 read_file 获取准确内容）", in.Path)
				}
				if count > 1 {
					return "", fmt.Errorf("old_string 在 %s 中出现 %d 次，请提供更多上下文使其唯一匹配", in.Path, count)
				}
				content = strings.Replace(content, in.OldString, in.NewString, 1)
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					return "", err
				}
				return fmt.Sprintf("Replaced 1 occurrence in %s", in.Path), nil
			}),
		Modes: []Mode{ModeBuild},
	}
}

// ---- delete ----

type deleteInput struct {
	Path string `json:"path" jsonschema:"description=要删除的文件路径"`
}

func deleteTool(cwd string) Tool {
	return Tool{
		GoAITool: goai.NewTool("delete", "删除指定文件。只能删除文件（删除目录请用 run_command rm，会请求确认）。",
			func(ctx context.Context, in deleteInput) (string, error) {
				if in.Path == "" {
					return "", fmt.Errorf("path 不能为空")
				}
				p := resolvePath(cwd, in.Path)
				info, err := os.Stat(p)
				if err != nil {
					return "", err
				}
				if info.IsDir() {
					return "", fmt.Errorf("%s 是目录，delete 只支持文件（删除目录请用 run_command rm）", in.Path)
				}
				if err := os.Remove(p); err != nil {
					return "", err
				}
				return "Deleted " + in.Path, nil
			}),
		Modes: []Mode{ModeBuild},
	}
}
