# zlite 详细设计（配置 / 会话 / 工具 / 提示词 / 上下文）

## 1. 配置 `~/.zlite/config.toml`

用 viper 加载（viper 内置 toml 解码），config 包转换为类型化 struct，**业务代码不直接接触 viper**。

```toml
# ~/.zlite/config.toml
[[providers]]                    # 一期只取第一个；二期扩展多个
  name = "default"
  type = "openai-compatible"     # 预留: openai / anthropic / ollama / ...
  base_url = "https://api.example.com/v1"
  api_key = "${ZLITE_DEFAULT_API_KEY}"   # 支持 ${ENV} 展开，密钥不落盘
  model = "gpt-4o"

[agent]
  mode = "plan"                  # 默认模式: plan | build
  auto_approve = false           # 信任模式（跳过确认）
  max_steps = 16                 # 单轮对话工具循环上限

[shell]
  confirm_commands = ["rm", "mv", "dd", "mkfs", "sudo", "chmod", "git", "git-push"]
```

### 1.1 config 包接口

```go
type Config struct {
    Providers []Provider `mapstructure:"providers"`
    Agent     AgentCfg   `mapstructure:"agent"`
    Shell     ShellCfg   `mapstructure:"shell"`
}
type Provider struct {
    Name    string `mapstructure:"name"`
    Type    string `mapstructure:"type"`
    BaseURL string `mapstructure:"base_url"`
    APIKey  string `mapstructure:"api_key"`
    Model   string `mapstructure:"model"`
}

func Load(path string) (*Config, error)        // viper 读取 + ENV 展开 + 默认值
func (c *Config) DefaultProvider() (*Provider, error)  // 一期: 取 providers[0]
```

- ENV 展开：`api_key` 等字段若为 `${VAR}` 形式，从环境变量取值；变量未设置则报错提示
- 默认值：`agent.mode=plan`、`agent.auto_approve=false`、`agent.max_steps=16`
- 支持命令行覆盖：`-m build` 优先于配置文件

## 2. 会话 jsonl 格式

存放：`~/.zlite/sessions/<cwd 哈希>/<时间戳>.jsonl`（cwd 用 SHA-256 前 12 位）

### 2.1 记录结构（每行一个 JSON 对象）

```jsonc
// 首行：会话元信息
{"type":"session","id":"20260813-1015-abc","cwd":"/data/apps/zlite","created_at":"RFC3339","model":"gpt-4o","provider":"default","mode":"plan","version":"0.1.0"}

// 消息
{"type":"message","id":"m1","role":"user","content":"...","ts":"..."}
{"type":"message","id":"m2","role":"assistant","content":"...","ts":"...","usage":{"input_tokens":1200,"output_tokens":340}}

// 工具调用与结果（按 call_id 配对）
{"type":"tool_call","id":"tc1","call_id":"call_1","name":"read_file","input":{"path":"main.go","offset":0,"limit":100},"ts":"..."}
{"type":"tool_result","id":"tr1","call_id":"call_1","output":"...","error":false,"duration_ms":12,"ts":"..."}

// 元事件（模式切换等）
{"type":"meta","event":"mode_change","value":"build","ts":"..."}
{"type":"meta","event":"auto_approve","value":true,"ts":"..."}
```

### 2.2 读写规则

- 追加写：每产生一条记录立即追加（`O_APPEND`），崩溃不丢数据
- 恢复（`zlite -c`）：读取该文件全部行，按顺序回放；`tool_call`/`tool_result` 按 `call_id` 配对，还原为模型所需的消息序列（assistant tool_call 消息 + tool 结果消息）
- `type=meta` 记录不参与模型上下文，仅用于 UI 展示与调试
- 文件保持单行独立可解析，`grep` 可直接检索历史

### 2.3 session 包接口

```go
type Manager struct{ ... }
func NewManager(globalDir string) *Manager
func (m *Manager) Create(cwd string, p *config.Provider, mode agent.Mode) (*Session, error)
func (m *Manager) Continue(cwd string) (*Session, error)   // 最近一个会话
func (m *Manager) List(cwd string) ([]SessionInfo, error)  // 二期: 交互选择
type Session struct {
    ID        string
    File      *os.File
    History   []Record     // 恢复后内存中的历史
}
func (s *Session) Append(r Record) error
func (s *Session) ToMessages() []llm.Message   // 转模型消息序列（含工具消息配对）
```

## 3. 工具定义与权限矩阵

| 工具 | 说明 | plan | build | 确认 |
|---|---|---|---|---|
| `read_file` | 读文件，参数 `path` / `offset` / `limit`，返回带行号内容；大文件分页 | ✓ | ✓ | — |
| `grep` | 正则搜索，参数 `pattern` / `path` / `-i`，返回 `file:line:text` | ✓ | ✓ | — |
| `glob` | 文件名匹配，参数 `pattern`，返回文件列表（排除 .git/node_modules） | ✓ | ✓ | — |
| `web_fetch` | HTTP GET 转文本（HTML 标签剥离），超时 30s、响应上限 1MB | ✓ | ✓ | — |
| `run_command` | 执行 shell；参数 `command` / `timeout_s`；工作目录=项目 cwd。**双模式注册**：plan 版（只读白名单）/ build 版（全量） | 只读白名单 | 全量 | 仅危险命令 |
| `write_file` | 创建/覆写文件（自动建父目录），参数 `path` / `content` | ✗ | ✓ | 直接执行 |
| `edit_file` | 精确字符串替换：`path` / `old_string` / `new_string`（必须唯一匹配，否则报错让模型重试） | ✗ | ✓ | 直接执行 |
| `delete` | 删除文件（目录用 run_command rm，会确认），参数 `path` | ✗ | ✓ | 直接执行 |
| `read_skill`（二期） | 按需读取 skill 正文 | ✓ | ✓ | — |

> 2026-08-13：确认机制按用户决策 D3 修订——写操作在 build 模式直接执行，不确认；仅危险 shell 命令（黑名单 + 危险参数模式）经 TUI 内联确认（`Approve? [y/n]`）。

### 3.1 run_command 的 plan 只读白名单

- 允许（前缀匹配 + 参数校验）：`ls pwd cat head tail wc find grep rg which file stat du df echo date whoami uname env git`
- git 只读子命令：`git status` `git diff` `git log` `git show` `git branch` `git remote`（其余 git 子命令在 plan 模式拒绝）
- 禁止的 shell 元字符：`>` `>>` `;` `&&` `||`（`|` 管道一期也禁止，避免绕过检测）
- 实现：`shlex` 分词后检查 argv[0] 白名单 + 参数列表扫描元字符；命中即返回"plan 模式不允许"错误

### 3.2 危险命令检测（build 模式，`run_command` 的 NeedApprove）

1. `argv[0]` 的 basename 命中 `shell.confirm_commands` 黑名单 → 需确认
2. 危险参数模式（正则）：
   - `rm -rf /`、`rm -rf ~` 等根/家目录递归
   - `:(){ :|:& };:` fork bomb
   - `git push` / `git reset --hard` / `git clean -f`（`git` 已在黑名单，此处兜底）
3. 命中后发 `ApprovalRequestEvent`（摘要 = 完整命令），等待 Approver

### 3.3 edit_file 语义

- `old_string` 必须在文件中**恰好出现一次**；0 次或多于 1 次均返回错误（附出现次数），模型自行调整后重试
- 应用前生成 unified diff 摘要（行号 + 变更行），作为确认层的预览内容
- diff 实现：优先引入轻量库（如 `github.com/hexops/gotextdiff`，Myers diff）；若嫌体积大，退化为直接展示 old→new 片段 + 行号

### 3.4 工具输出限制

- 所有工具输出上限 64KB，超出截断并标注 `[truncated]`，防止上下文爆炸

## 4. 系统提示词设计（`internal/agent/prompt.go`）

```text
你是 zlite，一个运行在终端里的轻量级编程助手。

环境：
- 操作系统: <GOOS>
- 工作目录: <cwd>
- 当前模式: plan（只读）| build（可写）

可用工具（plan 模式列出只读子集，build 模式列出全部）：
- read_file: ...
- grep: ...
- glob: ...
- web_fetch: ...
- run_command: 说明当前模式下哪些命令可用
- edit_file / delete: 仅 build 模式，修改前先 read 目标文件，替换必须精确唯一匹配

行为准则：
- 回答简洁，代码用 ```语言 代码块 包裹
- 优先用工具获取事实，不要凭记忆断言文件内容
- 在 plan 模式只做分析给出方案；在 build 模式可直接动手修改
- 执行破坏性操作前先说明意图
```

- 模式切换时（`/plan` `/build`）重新组装 system prompt
- 二期：skills 的描述列表注入此处，正文按需经 `read_skill` 读取

## 5. 上下文管理（`internal/agent/context.go`）

一期（简单策略）：

- 窗口截断：保留 system prompt + 最近 N 条消息（默认 40 条，可配），更早的丢弃并记录 meta 事件
- token 估算：按 `len(content)/4` 粗略估算（中文按 rune 计），不引 tokenizer 库
- 每轮完成后把模型返回的 `usage` 写入会话记录与状态栏

二期（细化）：

- 按估算 token 数截断而非条数
- 截断策略：保留首尾（system + 最近工具结果），中段做摘要占位
- 预留：接入 `llm` 层返回的真实 usage 校准估算

## 6. 错误处理约定

- 模型 API 错误：goai 返回类型化错误（`APIError` / `ContextOverflowError`），TUI 状态栏展示错误摘要，对话不崩溃
- 工具执行错误：作为工具结果字符串（`error: true`）返回模型，模型可自行修正
- 用户拒绝确认：同样作为工具结果（"用户拒绝了该操作"）返回模型
- 上下文溢出：`ContextOverflowError` 触发自动截断后重试一次，仍失败则提示用户开新会话
