# zlite 总体架构设计

## 1. 分层架构

```
┌─────────────────────────────────────────────────────┐
│ 前端层 Frontend                                      │
│   TUI (gocui)            ACP Agent (二期, acp-go-sdk) │  ← 可替换、可并存
└───────────────┬───────────────────────┬──────────────┘
                │ 事件流(events)        │ 事件流
┌───────────────▼───────────────────────▼──────────────┐
│ Agent 核心层（与 UI 完全无关）                         │
│   对话循环 / plan·build 模式 / 工具调度 / 上下文管理    │
│   Approver 接口 ← 确认回调（UI 无关）                  │
└───────────────┬───────────────────────────────────────┘
                │
┌───────────────▼───────────────────────────────────────┐
│ 能力层                                                │
│  llm(goai)  tools(6类)  session(jsonl)  skills  config │
└───────────────────────────────────────────────────────┘
```

依赖方向：`cmd/zlite` → `internal/agent` → `internal/{llm,tools,session,skills,config}`；`internal/tui` 与 `internal/acp` 只依赖 `internal/agent` 的事件契约，**不反向依赖**。

## 2. 目录结构

```
zlite/
├── cmd/zlite/
│   ├── main.go            # 入口：flag 解析（-m/-c/-l/acp 子命令）、启动 TUI
│   └── app.go             # 组装依赖（config → llm → tools → agent → tui）
├── internal/
│   ├── config/            # viper + toml 加载、ENV 展开、类型化 Config struct
│   ├── agent/
│   │   ├── agent.go       # Agent 核心：Run 循环、事件流、工具调度
│   │   ├── mode.go        # plan/build 模式、权限矩阵、Approver 接口
│   │   ├── events.go      # 事件类型定义（TUI/ACP 共享契约）
│   │   ├── context.go     # 上下文窗口管理（token 估算 + 截断）
│   │   └── prompt.go      # 系统提示词组装（角色 + 工具说明 + skills 注入）
│   ├── llm/               # goai 封装：compat provider、消息转换、流式、usage
│   ├── tools/
│   │   ├── registry.go    # 工具注册表 + 权限标记 + plan/build 过滤 + 危险检测
│   │   ├── read.go        # read_file / grep / glob（阅读类，全模式）
│   │   ├── edit.go        # edit_file / delete（build 模式，需确认）
│   │   ├── shell.go       # run_command + 只读白名单 + 危险命令检测
│   │   ├── web.go         # web_fetch（全模式）
│   │   └── skills.go      # read_skill 按需读取（二期）
│   ├── session/
│   │   ├── record.go      # jsonl 记录结构定义
│   │   ├── store.go       # jsonl 追加写、读取
│   │   └── manager.go     # 新建/继续/列表
│   ├── skills/            # SKILL.md 解析（frontmatter）、两级加载（二期）
│   ├── tui/
│   │   ├── tui.go         # gocui 布局与主循环
│   │   ├── chat_view.go   # 消息历史区（可滚动）
│   │   ├── input_view.go  # 输入区（gocui 默认 Editor，单行，Enter 提交）
│   │   ├── status_view.go # 状态栏：模式/模型/token 用量
│   │   ├── confirm_view.go# 内联确认层（diff 预览 + y/n）
│   │   └── render.go      # 轻量渲染（代码块着色）
│   ├── acp/               # 二期：ACP agent 适配（占位，仅注释契约）
│   └── version/           # 版本号注入
├── docs/
│   ├── plans/             # 本计划文档
│   └── acp-design.md      # 二期 ACP 接入设计（可并入 phase-2 或独立）
├── skills/                # 内置 skill 样例（可选）
├── Makefile               # build（-ldflags "-s -w"）/ test / lint
├── go.mod / go.sum
└── README.md
```

## 3. 关键抽象（ACP 预留的核心）

### 3.1 事件流契约 `internal/agent/events.go`

Agent 核心不直接接触 UI，只向外部发送事件。TUI 与 ACP 都是事件的消费者。

```go
type Event interface{ isEvent() }

type TextDeltaEvent struct{ Text string }            // 流式文本增量
type TextDoneEvent   struct{ FullText string }       // 单次回复结束（含完整文本）
type ToolCallEvent   struct {
    CallID  string
    Name    string
    Input   map[string]any
}
type ToolResultEvent struct {
    CallID string
    Name   string
    Output string
    Error  bool
}
type ApprovalRequestEvent struct {
    CallID  string
    Tool    string
    Summary string      // 确认摘要（如 diff 预览）
}
type ModeChangeEvent struct{ Mode Mode }
type DoneEvent struct{ Usage Usage }                 // 一轮对话结束（含 token 用量）
```

二期 ACP 实现时，把这些事件翻译为 ACP 的 update 消息（`text_delta` / `tool_call` / `permission_request` 等），无需改动 agent 核心。

### 3.2 Approver 接口 `internal/agent/mode.go`

```go
type ApprovalDecision int
const (
    Approved ApprovalDecision = iota
    Denied
)

type Approver interface {
    // Request 请求确认；返回是否批准。ctx 取消视为拒绝。
    Request(ctx context.Context, req ApprovalRequestEvent) (ApprovalDecision, error)
}
```

| 实现 | 场景 |
|---|---|
| `tuiApprover` | TUI 内联 y/n 确认 + diff 预览（confirm_view） |
| `autoApprover` | `agent.auto_approve = true` 时直接通过 |
| `acpApprover`（二期） | 转成 ACP permission update，等待客户端回复 |

### 3.3 工具注册表 `internal/tools/registry.go`

```go
type ToolMode int
const (
    ModePlan  ToolMode = iota // plan 模式可见
    ModeBuild                 // 仅 build 模式可见
)

type Tool struct {
    Name        string
    Description string          // 工具描述（注入 system prompt）
    Input       any             // 输入 struct（goai 反射生成 JSON Schema）
    Mode        ToolMode
    NeedApprove func(input any) (bool, string)  // 是否需确认 + 确认摘要
    Execute     func(ctx context.Context, input any) (string, error)
}
```

要点：

- **权限在注册表过滤**：agent 每次组装工具列表时按当前 mode 过滤 `Mode` 标记，工具自身不感知模式
- **确认在调度前**：`NeedApprove` 返回 true 时，agent 先发 `ApprovalRequestEvent`，调 `Approver.Request`，拒绝则把"用户拒绝"作为工具结果返回给模型（模型可调整方案）
- 危险命令检测复用同一机制：`run_command` 的 `NeedApprove` 内置黑名单 + 危险模式检测

### 3.4 Agent 核心循环 `internal/agent/agent.go`

```
Run(userMessage):
  1. 组装 system prompt（角色 + 工具说明 + skills 注入）+ 会话历史 + 用户消息
  2. llm.StreamText(...)  → 发射 TextDeltaEvent
  3. 若模型请求工具调用：
     a. 按 mode 校验工具可见性（不可见 → 错误结果返回模型）
     b. NeedApprove? → ApprovalRequestEvent → Approver.Request
     c. 执行工具 → ToolCallEvent / ToolResultEvent
     d. 工具结果追加进对话 → 回到 2（循环，上限 agent.max_steps）
  4. 完成 → DoneEvent（含 usage）→ 会话落盘
```

## 4. plan / build 权限模型

| 能力 | plan | build |
|---|---|---|
| `read_file` / `grep` / `glob` / `web_fetch` | ✓ | ✓ |
| `run_command` | 只读白名单（见 design.md） | 全量；危险命令需确认 |
| `edit_file` / `delete` | ✗（工具不注入） | ✓（默认需确认） |
| 模式切换 | TUI 内 `/plan` `/build`，会话 meta 记录 mode_change | 同左 |

system prompt 中写明各工具在两种模式下的可用性（模型自觉遵守），注册表再硬性过滤兜底。

## 5. TUI 布局（一期）

```
┌──────────────────────────────────────────────┐
│ 消息区 chat_view（可滚动，占主体）              │
│  ┌────────────────────────────────────────┐  │
│  │ 你: ...                                │  │
│  │ 助手: ...（流式增量）                    │  │
│  │ [工具] read_file ... ✓                 │  │
│  └────────────────────────────────────────┘  │
│ 输入区 input_view（1 行，Enter 提交）          │
│ 状态栏 status_view                           │
│  [plan] gpt-4o | ↑3.2k ↓1.1k | 输入 /help    │
└──────────────────────────────────────────────┘
```

键位（一期从简）：

| 键 | 行为 |
|---|---|
| Enter | 提交输入 |
| Ctrl+C | 退出（等价 `/exit`） |
| `/plan` `/build` `/exit` `/help` | 斜杠命令 |

交互细节：

- 流式输出：agent 事件循环在独立 goroutine 运行，通过 gocui 的 `g.Update(func())` 安全更新视图
- 工具调用展示：`[工具] <name> <参数摘要>` 一行，完成后显示 `✓` 或 `✗`
- 斜杠命令在输入区识别，命中后不进模型

## 6. CLI 接口（一期）

```
zlite                    # 新会话，默认 plan 模式
zlite -m build           # 指定初始模式
zlite -c                 # 继续最近会话
zlite -l                 # 列出会话（按项目）
zlite --version
```

二期追加：

```
zlite acp                # 以 ACP agent 模式运行（stdio）
zlite -m <model>         # 指定模型（多 provider 后）
```

## 7. 二期 ACP 接入路径（预演）

1. `internal/acp/` 实现 `acp.Agent` 接口（Initialize / NewSession / Prompt / 会话管理）
2. 复用 `internal/agent` 的核心循环：ACP 的 `Prompt` 请求 → 调 agent.Run → 把事件流翻译成 ACP update（`text_delta`、`tool_call`、`permission_request` 等）
3. `Approver` 用 acp 实现：通过 ACP permission 机制请求客户端确认
4. 传输：stdio（`acp.NewAgentSideConnection`），由 `zlite acp` 子命令启动
5. 工具的 `Execute` 全部可复用，无需改动
6. skills / 会话存储天然复用

详细设计见 phase-2.md。
