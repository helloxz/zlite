# zlite 开发计划（总览）

轻量级 CLI Coding Agent：体积小、内存占用低，面向 Linux / macOS，用于写代码场景。

- 模块路径：`github.com/helloxz/zlite`
- 二进制名：`zlite`
- 语言：Go（`go.mod` 声明 `go 1.25.0`，本地工具链 1.25.7，无需升级）
- 许可：沿用仓库现有 LICENSE（AGPL-3.0）

## 文档索引

| 文档 | 内容 |
|---|---|
| [architecture.md](./architecture.md) | 总体架构分层、目录结构、关键抽象（事件流 / Approver / 工具注册表）、ACP 预留 |
| [design.md](./design.md) | 配置 toml 格式、会话 jsonl 格式、工具与权限矩阵、危险命令清单、系统提示词、上下文管理 |
| [phase-1.md](./phase-1.md) | 阶段一：TUI 连续对话可跑通（详细任务 + 验收标准） |
| [phase-2.md](./phase-2.md) | 阶段二：plan/build 完整权限、skills、ACP 协议、多渠道多模型 |

## 已确认决策（ADR 记录，勿随意更改；如需变更先讨论）

### D1. AI SDK 选型：`github.com/zendev-sh/goai`（替换最初的 grafana/ai-sdk）

理由：

- grafana/ai-sdk 要求 `go 1.26.3`（本地 1.25.7 不满足，需升级工具链），且无正式 release（只有 pseudo-version，53 commits，快速迭代期）
- goai 要求 `go 1.25.0`，本地直接可用，无需升级
- goai 已发布 v0.9.0（有 changelog），242 commits，API 稳定
- goai 几乎 stdlib only（仅 `golang.org/x/oauth2` 间接依赖），最贴合"轻量、体积小"目标
- goai 提供 `compat` 包支持 OpenAI 兼容自定义端点（`/v1/chat/completions`），满足"openai 自定义端点 + chat 接口"需求
- goai 内置 `WithMaxSteps` 自动工具循环 + 生命周期 hooks（`WithOnBeforeToolExecute` / `WithOnToolCallStart` / `WithOnStepFinish` / `WithOnFinish`），是 plan/build 权限控制与 TUI 事件流的关键
- goai 内置 MCP client，后期可直接扩展
- 许可证 MIT（grafana/ai-sdk 为 Apache-2.0，两者均可接受）

风险对冲：llm 层是独立包（`internal/llm`），若 goai 出问题可低成本换回 ai-sdk，tools/session/tui 层不受影响。

### D2. 数据存放

- 全局配置：`~/.zlite/config.toml`
- 会话：`~/.zlite/sessions/<cwd 哈希>/<时间戳>.jsonl`（按项目隔离）
- skills 两级加载：
  - 全局：`~/.zlite/skills/`
  - 项目：`<项目根>/.zlite/skills/`
  - 兼容 Claude Code SKILL.md 格式（YAML frontmatter: `name` / `description` + Markdown body）

### D3. 确认机制（2026-08-13 修订）

- 修改 / 删除文件（`write_file` / `edit_file` / `delete`）：**build 模式下直接执行，无需确认**（用户决策修订；早期方案为内联确认 + diff 预览，已废弃）
- 执行 shell：build 模式下全量执行，**仅危险命令需要确认**（黑名单 `shell.confirm_commands` + 危险参数模式，如 `rm -rf /`）；确认在 TUI 聊天区内联进行（`Approve? ... [y/n]`），`auto_approve = true` 时自动批准
- plan 模式默认只读，无需确认

### D4. 危险命令清单（默认，可在配置中增删）

```toml
[shell]
confirm_commands = ["rm", "mv", "dd", "mkfs", "sudo", "chmod", "git", "git-push"]
```

- `git` 单独列出的原因：`git push` / `git reset --hard` 等属于破坏性/外发操作
- 危险命令检测是**提示性护栏，不是安全边界**（`/bin/rm` 等可绕过白名单），真正的确认权在人
- 另外做危险参数模式检测（如 `rm -rf /`、fork bomb 等）作为补充

### D5. TUI 消息渲染

轻量渲染：纯文本 + 代码块 / 行内代码简单着色，**不引入**第三方 markdown 渲染库，保持二进制小。

### D6. 多行输入

一期不做多行输入：输入区使用 gocui 默认 Editor（单行），Enter 提交。二期再考虑自写 Editor 支持多行（如 Alt+Enter 换行）。

### D7. 配置结构直接按多 provider 数组设计

`[[providers]]` 数组，一期代码只取第一个，二期扩展多渠道时**无需迁移配置格式**。

### D8. 交互方式

- 斜杠命令（`/plan`、`/build`、`/exit`、`/help`）而非抢占快捷键，可扩展性强（二期加 `/model`、`/sessions`、`/skills`）
- 快捷键仅保留基础键：Enter 提交、Ctrl+C 退出（或由斜杠命令 `/exit` 退出）

## 需求总览

1. 配置：toml + viper
2. 模型：一期单一渠道/单一模型（OpenAI 兼容端点，chat 接口），二期多渠道多模型
3. 平台：仅 Linux / macOS
4. 交互：一期 TUI 连续对话；二期 ACP 协议（预留好设计）
5. 模式：plan（只读）与 build（可写）两种模式
6. 工具函数：
   - 阅读（全模式）：`read_file` / `grep` / `glob`
   - 删除（build）：`delete`
   - 修改（build）：`edit_file`
   - 执行 shell（plan 只读白名单 / build 全量）：`run_command`
   - `web_fetch`（全模式）
7. 会话：jsonl 存储
8. skills：两级目录，兼容 Claude Code 格式

## 里程碑

- **M1（阶段一）**：`zlite` 可启动 → 连续对话 → 流式输出 → `zlite -c` 恢复上下文
- **M2（阶段二）**：plan/build 完整权限与确认、skills、ACP（`zlite acp`）、多渠道多模型
