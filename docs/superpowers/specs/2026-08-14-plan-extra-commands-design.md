# 设计：plan 模式自定义放行命令（[shell] plan_extra_commands）

日期：2026-08-14
状态：已批准，实现中

## 背景

plan 模式下 `run_command` 仅允许内置只读白名单（`internal/tools/shell.go`
`readOnlyCommands` + `gitReadOnlySubcommands`），硬编码不可扩展。用户希望
通过配置追加自定义命令（如 `python3`、`kubectl`），与内置白名单**追加合并并去重**。

## 决策（用户已确认）

1. 配置参数：`[shell] plan_extra_commands = ["python3", ...]`（与 confirm_commands 并列，
   按被配置对象内聚：两个参数都是 run_command 工具的执行策略）。
2. 放行语义：**仅放行命令名**，按 basename 匹配（与内置一致）；
   shell 元字符禁令（`> ; & | $ 反引号 换行`）、git 写子命令拦截等安全校验**全部不变**。
3. 合并方式：内置白名单 + 用户列表追加合并、去重（map 天然去重），trim 后忽略空项。

## 改动点

- `internal/config/config.go`：`ShellCfg` 新增 `PlanExtraCommands []string`（默认空）。
- `internal/tools/shell.go`：
  - `mergeReadOnlyCommands(extra []string) map[string]bool`：内置白名单 + 用户命令合并去重；
  - `validateReadOnlyCommand` 改为接收合并后的 map；
  - `runCommandPlanTool` 接收 extra，工具描述动态追加用户放行命令（无 extra 时描述与现状一致）。
- `internal/tools/registry.go`：`New` 增加 `planExtraCommands` 参数，透传给 plan 工具。
- 调用点：`cmd/zlite/app.go`、`internal/acp/acp.go` 传入 `cfg.Shell.PlanExtraCommands`。
- 测试：合并去重、放行生效、元字符仍拦截、git 写子命令仍拦截、config 解析。
- 文档：`config.example.toml`、`docs/config.md`、`README.md`、`docs/plans/design.md`。

## 安全边界（保持不变）

- 元字符禁令：`forbiddenShellChars` 不因本功能放宽；
- git 特判：子命令仍限只读列表，`plan_extra_commands` 无法放行 `git push`；
- 危险参数模式/plan 白名单之外的命令仍拒绝。
