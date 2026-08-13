package tools

import (
	"context"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/zendev-sh/goai"
)

const (
	defaultCmdTimeout = 30  // 秒
	maxCmdTimeout     = 120 // 秒
)

// readOnlyCommands 是 plan 模式下允许的命令白名单。
var readOnlyCommands = map[string]bool{
	"ls": true, "pwd": true, "cat": true, "head": true, "tail": true,
	"wc": true, "find": true, "grep": true, "rg": true, "which": true,
	"ps": true, "pgrep": true, "free": true, "uptime": true, "lsof": true,
	"file": true, "stat": true, "du": true, "df": true,
	"echo": true, "date": true, "whoami": true, "uname": true, "env": true,
}

// gitReadOnlySubcommands 是 plan 模式下允许的 git 只读子命令。
var gitReadOnlySubcommands = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "branch": true, "remote": true,
}

// gitDangerousSubcommands 是 build 模式下需要确认的 git 写操作子命令。
var gitDangerousSubcommands = map[string]bool{
	"push": true, "reset": true, "clean": true, "checkout": true,
	"merge": true, "rebase": true, "cherry-pick": true, "revert": true,
	"switch": true, "restore": true, "commit": true, "tag": true, "branch": true,
}

// shell 元字符（plan 模式一律禁止；含管道/重定向/拼接/命令替换都可能是写通道）。
var forbiddenShellChars = regexp.MustCompile(`[>;&|$\x60\n]`)

type runCommandInput struct {
	Command  string `json:"command" jsonschema:"description=要执行的命令"`
	TimeoutS int    `json:"timeout_s,omitempty" jsonschema:"description=超时秒数，默认 30，最大 120"`
}

// ---- plan 模式：只读白名单版 ----

func runCommandPlanTool(cwd string) Tool {
	return Tool{
		GoAITool: goai.NewTool("run_command", "执行 shell 命令。当前处于 plan 模式：仅允许只读命令（ls/pwd/cat/head/tail/wc/find/grep/rg/which/ps/pgrep/free/uptime/lsof/file/stat/du/df/echo/date/whoami/uname/env 及 git 只读子命令 status/diff/log/show/branch/remote），禁止重定向、管道、分号拼接等。",
			func(ctx context.Context, in runCommandInput) (string, error) {
				if in.Command == "" {
					return "", fmt.Errorf("command 不能为空")
				}
				if err := validateReadOnlyCommand(in.Command); err != nil {
					return "", err
				}
				return executeShell(ctx, cwd, in.Command, in.TimeoutS)
			}),
		Modes: []Mode{ModePlan},
	}
}

// ---- build 模式：全量执行 + 危险命令确认 ----

func runCommandBuildTool(cwd string, confirm []string) Tool {
	return Tool{
		GoAITool: goai.NewTool("run_command", "执行任意 shell 命令（工作目录为项目根）。危险命令（如 rm/mv/dd/mkfs/sudo/chmod、git 写操作等）会请求用户确认。",
			func(ctx context.Context, in runCommandInput) (string, error) {
				if in.Command == "" {
					return "", fmt.Errorf("command 不能为空")
				}
				return executeShell(ctx, cwd, in.Command, in.TimeoutS)
			}),
		Modes: []Mode{ModeBuild},
		NeedApprove: func(input map[string]any) (bool, string) {
			cmd, _ := input["command"].(string)
			if danger, reason := checkDangerousCommand(cmd, confirm); danger {
				return true, reason
			}
			return false, ""
		},
	}
}

// normalizeCommandTimeout 将模型传入的超时限制在安全范围内。
func normalizeCommandTimeout(timeoutS int) int {
	if timeoutS <= 0 {
		return defaultCmdTimeout
	}
	if timeoutS > maxCmdTimeout {
		return maxCmdTimeout
	}
	return timeoutS
}

// executeShell 执行命令并返回输出（超时控制）。
func executeShell(ctx context.Context, cwd, command string, timeoutS int) (string, error) {
	timeout := normalizeCommandTimeout(timeoutS)
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("command timed out (%d seconds)", timeout)
		}
		return "", fmt.Errorf("command failed: %v\noutput:\n%s", err, truncate(string(out)))
	}
	return string(out), nil
}

// validateReadOnlyCommand 校验命令符合 plan 模式只读约束。
func validateReadOnlyCommand(command string) error {
	if forbiddenShellChars.MatchString(command) {
		return fmt.Errorf("plan 模式不允许 shell 元字符（> ; & | $ 反引号 换行 等）: %q", command)
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return fmt.Errorf("空命令")
	}
	name := path.Base(fields[0])
	if readOnlyCommands[name] {
		return nil
	}
	if name == "git" {
		if len(fields) < 2 {
			return fmt.Errorf("git 需要子命令")
		}
		if gitReadOnlySubcommands[fields[1]] {
			return nil
		}
		return fmt.Errorf("plan 模式只允许 git 只读子命令（status/diff/log/show/branch/remote），不允许: %q", fields[1])
	}
	return fmt.Errorf("plan 模式不允许命令: %q", name)
}

// checkDangerousCommand 检测 build 模式下需要确认的危险命令。
// confirm 是黑名单（config [shell] confirm_commands）。
func checkDangerousCommand(command string, confirm []string) (bool, string) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false, ""
	}
	name := path.Base(fields[0])

	// 黑名单命令（git 特判：只读子命令不确认，写操作才确认）
	for _, c := range confirm {
		if c == name {
			if name == "git" {
				if len(fields) > 1 && gitDangerousSubcommands[fields[1]] {
					return true, "dangerous git command: " + command
				}
				return false, "" // git 只读操作（status/diff/log 等）不确认
			}
			return true, "dangerous command: " + command
		}
	}
	// git 写操作子命令（黑名单未覆盖时兜底）
	if name == "git" && len(fields) > 1 && gitDangerousSubcommands[fields[1]] {
		return true, "dangerous git command: " + command
	}
	// 危险参数模式
	if strings.Contains(command, "rm -rf /") || strings.Contains(command, "rm -rf ~") ||
		strings.Contains(command, ":(){") || strings.Contains(command, "mkfs") {
		return true, "dangerous pattern: " + command
	}
	return false, ""
}
