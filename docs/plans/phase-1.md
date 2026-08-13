# 阶段一：TUI 连续对话（M1）

目标：`zlite` 可启动 → 连续对话（流式输出）→ 会话 jsonl 落盘 → `zlite -c` 恢复上下文。

范围：plan 模式的只读能力 + 对话循环 + 会话存储 + TUI 基础交互。**不包含**：文件修改、危险命令确认、skills、ACP、多渠道。

## 1. 任务清单（按依赖顺序）

### T1. 项目脚手架

- [ ] `go mod init github.com/helloxz/zlite`，`go.mod` 声明 `go 1.25.0`
- [ ] 引入依赖（以 `go get` 最新稳定为准）：
  - `github.com/zendev-sh/goai`（v0.9.0，含 `provider/compat` 子包）
  - `github.com/awesome-gocui/gocui`（v1.x）
  - `github.com/spf13/viper`（内置 toml 支持）
- [ ] `Makefile`：
  - `make build`：`go build -ldflags "-s -w -X github.com/helloxz/zlite/internal/version.Version=$(VERSION)" -o bin/zlite ./cmd/zlite`
  - `make test`、`make lint`（golangci-lint 可选）、`make run`
- [ ] `README.md`（简要用法）、`.gitignore`（bin/、.zlite/）
- [ ] `internal/version` 包

### T2. config 包

- [ ] `Config` / `Provider` / `AgentCfg` / `ShellCfg` / `TUISet` / `SessionCfg` struct（见 design.md §1.1）
- [ ] `Load(path)`：viper 读 `~/.zlite/config.toml`（文件不存在时用默认值 + 首次运行提示模板），`${ENV}` 展开，默认值填充
- [ ] `DefaultProvider()`：取 `providers[0]`，一期仅支持 `type = "openai-compatible"`
- [ ] 单元测试：默认值、ENV 展开、非法配置报错

### T3. llm 包（goai 封装）

- [ ] `BuildModel(p *config.Provider) (provider.LanguageModel, error)`：`compat.New(compat.WithBaseURL, compat.WithAPIKey)`，模型 ID 用 `p.Model`
- [ ] `Message` 类型与 `ToGoAIMessages(...)` 转换：user / assistant / tool 消息 → goai `provider.Message`（tool 结果走 `tool_result` part）
- [ ] `StreamText(ctx, model, system, messages, tools, opts) (stream, error)`：封装 `goai.StreamText` + `WithMaxSteps(agent.max_steps)` + hooks 透出事件（TextDelta / ToolCall / ToolResult / usage）
- [ ] usage 提取：从结果 `Response.Usage` 读取 input/output tokens
- [ ] 验证：写一个最小 `main` 或测试，连真实端点冒烟（可选，依赖用户 API key）

### T4. tools 包（只读子集 + build 写能力，2026-08-13 扩展）

- [ ] `registry.go`：`Tool` struct（Name/Description/Input/Mode/NeedApprove/Execute）、`Registry` 注册表、`ForMode(mode)` 过滤
- [ ] `read.go`：
  - `read_file`：`path` / `offset` / `limit`，返回带行号内容；越界安全；文件过大截断
  - `grep`：`pattern`（RE2）/ `path`（默认 cwd）/ 大小写选项，返回 `file:line:text`，上限 200 条
  - `glob`：`pattern`，排除 `.git` / `node_modules` / `.zlite`
- [ ] `web.go`：`web_fetch`：GET + 30s 超时 + 1MB 上限 + HTML 标签剥离转纯文本
- [ ] `shell.go`（一期即实现 plan 白名单，因为 plan 是默认模式）：
  - shlex 分词、argv[0] 白名单、禁元字符 `> >> ; && || |`、git 只读子命令白名单
  - 工具描述中说明 plan 限制
- [ ] 工具输出 64KB 截断（统一 helper）
- [ ] 单测：read_file 边界、grep/glob 行为、shell 白名单拒绝用例（`rm`、`cat a > b`、`git push`）

### T5. session 包

- [ ] `record.go`：Record 类型 + JSON 编解码（`session` / `message` / `tool_call` / `tool_result` / `meta` 五类）
- [ ] `store.go`：`~/.zlite/sessions/<cwd-hash>/<ts>.jsonl` 创建与 `O_APPEND` 追加
- [ ] `manager.go`：`Create` / `Continue`（按 mtime 取最近）/ `List`；`ToMessages()` 把历史转模型消息（tool_call/tool_result 按 call_id 配对还原）
- [ ] 单测：写读回放、配对还原、cwd 哈希目录

### T6. agent 核心

- [ ] `events.go`：事件类型（TextDelta / TextDone / ToolCall / ToolResult / ModeChange / Done + Usage）
- [ ] `mode.go`：`Mode`（plan/build）、`Approver` 接口、一期 `autoApprover`（读配置）+ `nilApprover`（拒绝一切）兜底
- [ ] `prompt.go`：系统提示词组装（见 design.md §4），含当前模式与工具清单
- [ ] `context.go`：窗口截断（保留 system + 最近 40 条，可配）
- [ ] `agent.go`：`Run(ctx, userMsg) error`——消息追加 → StreamText → 事件通道发射 → 工具调用循环（max_steps）→ usage → Done；每步落盘到 session
- [ ] `Agent` struct 组装：`New(cfg, llm, registry, session, approver, events chan)`
- [ ] 单测：mock llm（接口化，可注入 fake model）验证工具循环、模式过滤、max_steps、截断

### T7. TUI 包

- [ ] `tui.go`：gocui 初始化（`gocui.NewGui`）、layout（消息区/输入区/状态栏）、主循环、Ctrl+C 退出
- [ ] `chat_view.go`：消息历史渲染（用户/助手/工具行）、自动滚动到底部、流式增量更新（`g.Update`）
- [ ] `input_view.go`：单行输入，Enter 提交；识别斜杠命令 `/plan` `/build` `/exit` `/help`
- [ ] `status_view.go`：`[plan|build] 模型名 | ↑输入 ↓输出 token | 提示`
- [ ] `render.go`：轻量渲染——代码块（``` 包裹）着色 + 行内代码 `反引号` 着色，其余纯文本；不引 markdown 库
- [ ] `confirm_view.go`：一期只留接口占位（Approver 由 autoApprover/nilApprover 顶替），二期实现内联确认
- [ ] agent 事件循环 goroutine + `g.Update` 安全更新

### T8. main.go / app.go 组装

- [ ] flag：`-m plan|build`、`-c`（继续最近会话）、`-l`（列出会话）、`--version`
- [ ] 组装顺序：config → llm → tools registry → session manager → agent → tui
- [ ] 信号处理：SIGINT/SIGTERM 优雅退出（会话落盘）

### T9. 文档收尾

- [ ] README：安装（`make build`）、配置示例、快捷键、斜杠命令
- [ ] 把本阶段实际实现与计划的差异回写到 docs/plans（保持计划文档为活文档）

## 2. 验收标准（M1 完成定义）

1. `make build` 通过，`bin/zlite --version` 输出版本
2. 首次运行自动提示创建 `~/.zlite/config.toml`；配置了 base_url/api_key/model 后启动成功
3. TUI 启动：消息区/输入区/状态栏正确布局，`[plan]` 模式显示
4. 输入自然语言 → 流式输出实时显示 → 完整回复渲染（代码块着色）
5. 输入 `/build` 或按 Tab → 状态栏切换为 `[build]`；build 模式下模型可调用 write_file/edit_file/delete 创建与修改文件（直接执行，无需确认）
6. 问"列出当前目录文件"→ 模型调用 `run_command ls` 或 `glob`，工具行显示 `[ok]`
6b. build 模式下执行危险命令（如 rm/mv/git push）→ TUI 弹出 `Approve?` 提示，输入 y 执行 / n 拒绝；auto_approve=true 时自动执行
7. 退出后 `zlite -c` 恢复上次对话上下文，继续提问模型能理解前文
8. `~/.zlite/sessions/<hash>/<ts>.jsonl` 内容完整可读（session/message/tool_call/tool_result 各行齐全）
9. `Ctrl+C` 正常退出，无残留 goroutine 报错
10. 体积检查：`bin/zlite` 二进制（`-s -w`）目标 < 20MB（实测 8.1M，2026-08-13）

## 3. 一期明确不做（防止范围蔓延）

- skills（含 read_skill）
- ACP（`zlite acp`）
- 多渠道 / 多模型切换（`/model`）
- 多行输入、markdown 完整渲染、会话列表交互选择

> 2026-08-13 更新：文件修改/删除工具（write_file/edit_file/delete）、build 模式全量
> shell 与危险命令 TUI 确认（tuiApprover）已提前实现（见 T4 扩展），从二期 P1 移入一期。
> 按用户决策 D3 修订：**build 模式下写操作（write/edit/delete）直接执行，不确认**；
> 仅危险 shell 命令（confirm_commands 黑名单 + 危险模式）需确认。

### T10. /new 新建会话（2026-08-13 追加）

- [x] `agent.SetSession(sess)`：关闭旧会话句柄并切换（会话实时落盘，切换安全）
- [x] TUI `/new` 命令：`newSession` 回调注入（app.go 组装 mgr.Create + SetSession，TUI 保持零业务）；chat 区重置并提示 "New session started"；**模式重置为 plan**（用户决策）；busy 时提示等待
- [x] helpText 补充 `/new`；测试：SetSession 切换/新会话对话落盘/交互 /new 分支（回调调用 + 提示 + 模式重置）

### T11. /init 项目初始化 + AGENTS.md 自动加载（2026-08-13 追加）

- [x] `agent.runOnce` 提取：Run/RunInit 共用核心循环（截断/请求/事件/落盘）
- [x] `initSystemPrompt` 指令模板（方法论参考 Reasonix init skill）：已有 AGENTS.md 改进而非覆盖、探索清单（manifest/README/结构/代表源码）、验证命令真实存在、六节结构（Title/Project/Commands/Architecture/Conventions/Notes）、精简不含密钥
- [x] TUI `/init`：plan 模式输出内容到聊天区 + 提示切 build 重试；build 模式直接 write_file 写入
- [x] AGENTS.md 自动加载：`loadProjectContext(cwd)`（仅当前目录、>64KB 截断），每次 Run 组装 system prompt 时实时读取（/init 或手改后立即生效，无需重启）；config `agent.load_agents_md`（默认 true）；注入格式 `## Project Context (AGENTS.md)`
- [x] 测试：加载/截断/注入/开关关闭/RunInit（plan 与 build 指令）/交互 /init 分支
