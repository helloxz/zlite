# 阶段二：plan/build 完整权限、skills、ACP、多渠道（M2）

目标：成为可实际写代码的 agent——能改文件、能安全执行命令、支持 skills 扩展、通过 ACP 协议接入编辑器、支持多渠道多模型。

## 1. 任务清单（建议顺序：P1 → P4 独立可并行）

### P1. plan/build 完整权限与确认机制

> 2026-08-13：本阶段大部分内容已提前完成并移入一期（见 phase-1.md T4 扩展）：
> - ✅ write_file / edit_file（精确唯一匹配替换）/ delete —— **build 模式直接执行，不确认**（用户决策 D3 修订）
> - ✅ run_command 双模式（plan 只读白名单 / build 全量 + 危险命令检测）
> - ✅ 危险命令 TUI 内联确认（tui.Approver，y/n 决策，无需覆盖层）
> - ✅ 用户拒绝 → "用户拒绝了该操作" 返回模型，模型可调整方案
>
> 剩余可选项（不再必需，因写操作不确认）：
- [ ] （可选）写操作前的 unified diff 预览（如未来恢复确认机制再引入 `gotextdiff`）

### P2. skills（兼容 Claude Code 格式）

- [ ] `internal/skills`：
  - `SKILL.md` 解析：YAML frontmatter（`name` / `description`，可加 `allowed-tools` 等扩展字段）+ Markdown body；无 frontmatter 的目录跳过
  - 两级加载：全局 `~/.zlite/skills/` + 项目 `<cwd>/.zlite/skills/`（项目优先，同名覆盖）；递归扫描 `**/SKILL.md`
- [ ] 注入策略：全部 skill 的 `name + description` 列表注入 system prompt；正文按需读取
- [ ] `tools/skills.go`：`read_skill` 工具（参数 `name`，返回 SKILL.md 全文），全模式可用
- [ ] `/skills` 斜杠命令：列出已发现 skills（含来源目录）
- [ ] 单测：frontmatter 解析、两级加载优先级、同名覆盖
- [ ] 内置示例 skill（如 `git-workflow`）放 `skills/` 目录作为样例

### P3. 会话管理增强

- [ ] `zlite -l` 交互式会话列表选择（方向键 + Enter，复用 confirm_view 的覆盖层模式）
- [ ] 上下文管理细化：按估算 token 截断（保留 system + 最近工具结果，中段摘要占位）；用模型返回的真实 usage 校准估算
- [ ] `session.keep` 生效：超出保留数的旧会话文件归档到 `.archive/`
- [ ] 多行输入（可选优化）：自写 gocui Editor，Alt+Enter 换行、Enter 提交

### P4. ACP 协议支持（核心预留落地）

依赖：`github.com/coder/acp-go-sdk`（v0.13.5+）

- [ ] `internal/acp/` 包：
  - 实现 `acp.Agent` 接口：`Initialize`（返回能力声明，广告 `_meta` 扩展）、`NewSession` / `LoadSession`、`Prompt`（入口：把 ACP 请求转成 agent.Run 调用）、会话生命周期
  - `acpApprover`：`ApprovalRequestEvent` → ACP `permission_request` update，等待客户端 `permission` 响应
  - 事件翻译层：agent 事件流 → ACP update（`text_delta`、`tool_call`、`tool_result`、`permission_request`、`session_updated` 等）
  - 工具全部复用 `internal/tools` 注册表，零改动
- [ ] `cmd/zlite` 增加 `zlite acp` 子命令：`acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)` + `Start`，stdio 传输
- [ ] ACP 会话映射：ACP session ↔ `~/.zlite/sessions/` jsonl（ACP 的 `session_id` 记入 jsonl 首行 meta，恢复时按 id 查找）
- [ ] 验收：用 `acp-go-sdk` 自带 `example/client` 或 `zlite acp` + 手写小客户端打通一轮对话（含一次工具调用 + 一次权限请求）
- [ ] 文档：`docs/acp-design.md` 记录映射细节（事件→update 对照表）

### P5. 多渠道 / 多模型

> 2026-08-13：单渠道多模型与 type 分派已提前完成并移入一期（见 phase-1.md P11）：
> - ✅ `models` 数组（默认第一个）、type `厂商.协议` 分派（openai.chat / openai.responses）
> - ✅ `~/.zlite/.env` 自动加载（godotenv）
> 本项剩余：多渠道选择与运行时切换。

- [ ] config `[[providers]]` 完整走通：`DefaultProvider()` 改为按 `name` 选择
- [ ] `/model` 斜杠命令：列出 providers，`/model <name>` 切换（切换时重新 BuildModel + 更新状态栏 + 会话 meta 记录）
- [ ] type 扩展：`anthropic`、`google`、`ollama` 等厂商走 goai 各 provider 包（`provider/anthropic`、`provider/google` 等），llm 包 `BuildModel` 分派表追加一行即可
- [ ] 单测：多 provider 配置解析、切换逻辑

## 2. 验收标准（M2 完成定义）

1. build 模式：模型可修改文件，修改前 TUI 弹出 diff 预览，`y` 应用 / `n` 拒绝；`auto_approve=true` 时无打断直接应用
2. build 模式：`rm`、`mv`、`git push` 等危险命令触发确认；普通命令（`ls`、`go build`）直接执行；plan 模式写命令被拒
3. 用户拒绝后模型能调整方案（工具结果可见）
4. skills：全局与项目两级目录的 skill 均被发现，`/skills` 可列出，模型可按需调用 `read_skill` 并遵循其中指令
5. `zlite acp`：标准 ACP 客户端可初始化、发起对话、接收流式文本与工具调用、响应权限请求，全程无崩溃
6. `/model` 可在多个 provider 间切换，状态栏与后续请求使用新模型
7. 上下文截断在长对话中生效（meta 事件记录截断），会话可完整恢复
8. 全量测试通过（`make test`），无数据竞争（`-race`）

## 3. 二期明确不做（防止范围蔓延）

- Windows 支持（保持 Linux/macOS）
- MCP server 集成（goai 有 mcp client，但接入排在二期之后，作为三期候选）
- 多 agent / 子代理编排
- 会话加密、团队共享
- 图形化 diff（终端内字符级 diff 可视化为可选增强）

## 4. 三期候选（不承诺）

- MCP client（goai 内置，接入成本低）
- 流式 token 计费统计
- `--non-interactive` / 管道输入模式（脚本化）
- 内置 skill 市场下载
