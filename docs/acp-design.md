# zlite ACP 设计（docs/acp-design.md）

zlite 通过 ACP（Agent Client Protocol，`github.com/coder/acp-go-sdk` v0.13.5）以 stdio 方式接入编辑器等客户端：`zlite --acp` 或 `zlite acp`（两者等价）。

## 1. 架构

```
ACP client (editor)  ←──stdio──→  cmd/zlite runACP
                                      │
                                      ▼
                              internal/acp.Agent (acp.Agent 接口实现)
                                │ 每个 ACP session 一个 sessionState
                                │   ├─ *session.Session (jsonl 落盘)
                                │   ├─ *agent.Agent（独立实例）
                                │   ├─ acpApprover（permission 确认）
                                │   └─ 事件翻译 goroutine
                                │
                      ┌─────────┴──────────┐
                      ▼                    ▼
              tools.Registry（零改动）  llm.Streamer（复用）
```

- 多 ACP session 并存：每个 session 一个独立 `agent.Agent`（各自的 streamer/events/approver/**工具注册表**/**skills**，均按会话 cwd 构建），互不干扰；`session.Manager` 只读共享。
- 请求并发模型：SDK 对带 ID 的请求（prompt/set_mode 等）并发处理，notification（cancel）走独立串行队列——busy 检查 + per-session cancel 足以保证正确性。
- ACP 通信独占 stdout；zlite 自身日志只走 stderr（入口已保证）。

## 2. 会话映射

- ACP `session_id` **直接复用 zlite 会话 id**（`~/.zlite/sessions/<cwd-hash>/<id>.jsonl`），创建时以 `meta` 记录 `acp_session_id` 留痕。
- **会话 cwd 由 client 指定**（`session/new` / `session/load` / `session/resume` 请求的 `Cwd` 字段）：
  - 校验：必须是**存在的绝对目录路径**，否则报错；空值回退进程 cwd（防御）。
  - 工具注册表（路径解析、`run_command` 执行目录）、项目 skills（`<cwd>/.zlite/skills/`）、AGENTS.md 均按**会话 cwd** 构建——每会话独立，互不影响；全局 skills 目录不变。
  - 会话文件按 cwd 哈希分区：client 须用与创建时一致的 cwd（协议语义），不一致时 "session not found"（含内存中已打开会话的 cwd 比对）。
- `session/list` → 按请求的 `Cwd` 过滤（未指定时列进程 cwd）；目录不存在返回空列表（查询语义不报错）。
- **信任边界**：允许任意绝对路径（用户决策）——build 模式下 `write_file/edit_file/delete` 直接执行不确认，失控 client 可让 zlite 写任意目录（限于启动用户权限）；危险 `run_command` 仍走 `permission_request`。

## 3. 事件 → update 对照表

| agent 事件 | ACP update | 说明 |
|---|---|---|
| `TextDeltaEvent` | `agent_message_chunk` | 流式文本增量 |
| `ToolCallEvent` | `tool_call` | status=in_progress，kind 按工具名映射（read/search/execute/edit/delete/fetch/other） |
| `ToolResultEvent` | `tool_call_update` | completed / failed，content 为输出文本 |
| `ModeChangeEvent` | `current_mode_update` | session/set_mode 或加载会话时触发 |
| `ThinkingStartEvent` | （不映射） | 思考内容不落盘，无增量可发 |
| `DoneEvent` | （不映射） | 结束以 Prompt 响应 `stopReason` 表达 |
| `ApprovalRequest`（Approver 接口） | `permission_request` | 同步等待 client 响应；`allow_once` / `reject_once` 两个选项；ctx 取消视为拒绝 |

## 4. 能力声明（Initialize）

- `loadSession = true`；`sessionCapabilities`：list / close / resume。
- 协议版本：`ProtocolVersionNumber`（协商交给 SDK）。
- `AgentInfo`：zlite + `version.String()`。
- `Authenticate` / `Logout`：无鉴权，空响应。

## 5. 会话模式与配置选项

| 项 | 机制 | 值 | 切换方法 |
|---|---|---|---|
| mode（plan/build） | 官方稳定 `Session Modes`（`NewSessionResponse.Modes`） | `{plan, build}` | `session/set_mode` → `agent.SetMode` |
| model 列表 | `session/config` select（**UNSTABLE**） | `providers[0].models` | `session/set_config_option` → `llm.BuildModelNamed` + `agent.SetStreamer` |
| thinking 强度 | `session/config` select（**UNSTABLE**） | `[none, auto, low, medium, high, xhigh, max]` | 同上 → `agent.SetThinking` |

- config option 使用官方语义分类：`model` / `thought_level`。
- 切换类调用在 turn 进行中（busy）时返回错误（与 TUI busy 拒绝语义一致），由 client 重试。
- **风险**：`session/config` 相关类型在 SDK 中标记 UNSTABLE（协议演进中）；v0.13.5 已生成完整代码且 claude-code 等实现使用，升级 SDK 时需回归此部分。

## 6. 可用命令

- 创建/加载会话后推送一次 `available_commands_update`：仅 `init`（对应 `/init`，命令名不带斜杠，斜杠由 client 展示层添加；`input` 为可选参数 `hint`）。
- 执行通道：client 把命令作为普通文本发给 `session/prompt`（`"/init"` 或 `"/init <要求>"`），agent 检测前缀后走 `RunInit`，与 TUI `/init` 行为一致（整条消息记入会话）。

## 7. 权限确认

- `auto_approve = true`：直通（`agent.NewApprover(true)`）。
- 否则 `acpApprover.Request`：构造 `permission_request`（toolCall 含 id/kind/title=确认摘要/rawInput=工具参数）→ 同步等待；`allow_once` → 执行，`reject_once` / cancelled / ctx 取消 → 拒绝，拒绝原因作为工具结果返回模型（模型可调整方案）。
- `ApprovalRequest` 新增 `Input` 字段（TUI 确认不消费，ACP 展示用）。

## 8. 取消与关闭

- `session/cancel` → 取消该会话当前 turn 的 context；模型调用中断后 Prompt 返回 `stopReason=cancelled`。
- **并发 prompt 语义（SDK 内建）**：同一 session 的新 prompt 到达时，SDK（`sessionCancels`）自动取消旧 turn 的请求 ctx——最新 prompt 优先。zlite 侧不拒绝并发，而是等待旧 turn 清理完毕（`waitIdle`）后开始新 turn；等待期间自身 ctx 被取消（被更新的 prompt 取代）则以 `cancelled` 结束。
- `session/close` → 取消进行中的 turn（最多等 `closeWaitTimeout`）→ 停止翻译 goroutine → 关闭 jsonl 文件；turn 未在超时内结束时**不强制关闭文件句柄**（避免 turn 续写已关闭文件导致 panic，留给进程退出回收）；重复 close 幂等。

## 9. 验收（已通过）

1. 标准 ACP 客户端（io.Pipe 双端 + 真实 `bin/zlite --acp`）初始化、建会话、流式文本、工具调用（write_file 落盘）、危险命令权限请求（allow 后执行成功）、模式/模型/思考强度切换、会话 list/close，全程无崩溃。
2. `make test` 全绿，`go test -race ./internal/acp/` 无竞争。
3. 注意：验收时模型可能拒绝执行危险命令（模型自身安全行为），权限链路以单测 + 非拒绝场景（mv）验证。

## 10. 已知限制

- 仅文本 prompt（image/audio/resource block 忽略）；`embeddedContext` 未声明。
- `McpServers`、`additionalDirectories` 不支持（请求中静默忽略）。
- 模型列表一期仅取 `providers[0]`（多渠道见 P5）。
- `session/fork`、`session/delete` 未声明（不做）。