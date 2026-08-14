package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"strings"
	"sync"
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
	"ss": true, "netstat": true,
	"file": true, "stat": true, "du": true, "df": true,
	"echo": true, "date": true, "whoami": true, "uname": true, "env": true,
}

// cleanExtraCommands 去除用户额外命令中的空白并忽略空项（与 merge 口径一致）。
func cleanExtraCommands(extra []string) []string {
	var out []string
	for _, c := range extra {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// mergeReadOnlyCommands 合并内置只读白名单与用户额外放行命令（map 天然去重）：
// 返回新 map，不修改内置 readOnlyCommands。
func mergeReadOnlyCommands(extra []string) map[string]bool {
	allowed := make(map[string]bool, len(readOnlyCommands)+len(extra))
	for name := range readOnlyCommands {
		allowed[name] = true
	}
	for _, c := range cleanExtraCommands(extra) {
		allowed[c] = true
	}
	return allowed
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

func runCommandPlanTool(cwd string, extra []string) Tool {
	allowed := mergeReadOnlyCommands(extra)
	desc := "执行 shell 命令。当前处于 plan 模式：仅允许只读命令（ls/pwd/cat/head/tail/wc/find/grep/rg/which/ps/pgrep/free/uptime/lsof/ss/netstat/file/stat/du/df/echo/date/whoami/uname/env 及 git 只读子命令 status/diff/log/show/branch/remote），禁止重定向、管道、分号拼接等。"
	if extraCmds := cleanExtraCommands(extra); len(extraCmds) > 0 {
		// 动态追加用户额外放行命令，让模型知道可用（无额外命令时描述与内置一致）
		desc += " 用户额外放行: " + strings.Join(extraCmds, "/") + "。"
	}
	desc += shellHint()
	return Tool{
		GoAITool: goai.NewTool("run_command", desc,
			func(ctx context.Context, in runCommandInput) (string, error) {
				if in.Command == "" {
					return "", fmt.Errorf("command 不能为空")
				}
				if err := validateReadOnlyCommand(in.Command, allowed); err != nil {
					return "", err
				}
				return executeShell(ctx, cwd, in.Command, in.TimeoutS)
			}),
		Modes: []Mode{ModePlan},
	}
}

// ---- build 模式：全量执行 + 危险命令确认 ----

func runCommandBuildTool(cwd string, confirm []string) Tool {
	desc := "执行任意 shell 命令（工作目录为项目根）。危险命令（如 rm/mv/dd/mkfs/sudo/chmod、git 写操作等）会请求用户确认。" + shellHint()
	return Tool{
		GoAITool: goai.NewTool("run_command", desc,
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
// 非 Windows 平台用 sh -c；Windows 上优先探测 Git Bash/MSYS2 的 sh（POSIX 命令原样可用），
// 找不到则回退 cmd.exe /C（此时工具描述已提示模型改用 Windows 命令语法）。
func executeShell(ctx context.Context, cwd, command string, timeoutS int) (string, error) {
	timeout := normalizeCommandTimeout(timeoutS)
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	prefix := shellPrefix()
	args := make([]string, 0, len(prefix)+1)
	args = append(args, prefix[1:]...) // "-c"（或 cmd 的 "/C"）
	args = append(args, command)
	cmd := exec.CommandContext(cmdCtx, prefix[0], args...)
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

// windowsShellCandidates 是常见 Git Bash / MSYS2 的 sh 安装路径（探测顺序）。
var windowsShellCandidates = []string{
	`C:\Program Files\Git\bin\sh.exe`,
	`C:\Program Files\Git\usr\bin\sh.exe`,
	`C:\Program Files (x86)\Git\bin\sh.exe`,
	`C:\msys64\usr\bin\sh.exe`,
	`C:\tools\msys64\usr\bin\sh.exe`,
}

// detectShell 探测可用的 shell 解释器，返回 argv 前缀（如 {"sh","-c"} 或 {"cmd.exe","/C"}）。
// 非 Windows 平台固定使用 sh；Windows 上依次尝试 PATH 中的 sh 与常见 Git Bash/MSYS2
// 安装路径，全部失败则回退 cmd.exe /C（POSIX 命令不可用，由 shellHint 提示模型）。
// lookPath/fileExists 参数可注入，便于单测覆盖 Windows 分支。
func detectShell(goos string, lookPath func(string) (string, error), fileExists func(string) bool) []string {
	if goos != "windows" {
		return []string{"sh", "-c"}
	}
	if p, err := lookPath("sh"); err == nil {
		return []string{p, "-c"}
	}
	for _, p := range windowsShellCandidates {
		if fileExists(p) {
			return []string{p, "-c"}
		}
	}
	return []string{"cmd.exe", "/C"}
}

var (
	shellOnce sync.Once
	shellArgv []string
)

// shellPrefix 返回当前平台的 shell 前缀（首次调用时探测并缓存，避免每次命令执行重复探测）。
func shellPrefix() []string {
	shellOnce.Do(func() {
		shellArgv = detectShell(runtime.GOOS, exec.LookPath, func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		})
	})
	return shellArgv
}

// shellHint 返回注入 run_command 工具描述的 shell 类型提示。
// sh 可用时返回空（模型默认 POSIX 语法即可）；回退 cmd.exe 时提示改用 Windows 命令。
func shellHint() string {
	return shellHintFor(shellPrefix())
}

// shellHintFor 根据 shell argv 前缀生成提示文案（独立成函数便于测试）。
func shellHintFor(prefix []string) string {
	base := strings.ToLower(shellBaseName(prefix[0]))
	if base == "sh" || base == "sh.exe" {
		return ""
	}
	return " 当前 shell 为 Windows cmd.exe（未检测到 sh）。POSIX 命令（ls/cat/grep/find 等）不可用，请改用 Windows 命令（dir/type/findstr 等）。"
}

// shellBaseName 兼容两种路径分隔符地取文件名（Windows 路径 `C:\...\sh.exe` 在
// 非 Windows 平台上用 filepath.Base 会取不到文件名，这里统一转 `/` 再取）。
func shellBaseName(name string) string {
	return path.Base(strings.ReplaceAll(name, `\`, "/"))
}

// validateReadOnlyCommand 校验命令符合 plan 模式只读约束。
// allowed 是合并后的白名单（内置 + 用户额外放行，见 mergeReadOnlyCommands）。
func validateReadOnlyCommand(command string, allowed map[string]bool) error {
	if forbiddenShellChars.MatchString(command) {
		return fmt.Errorf("plan 模式不允许 shell 元字符（> ; & | $ 反引号 换行 等）: %q", command)
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return fmt.Errorf("空命令")
	}
	name := path.Base(fields[0])
	if allowed[name] {
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
