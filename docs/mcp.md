# MCP 配置与使用说明

[MCP（Model Context Protocol）](https://modelcontextprotocol.io) 是 AI 应用的
工具接入标准。zlite 通过 MCP 把外部 server 提供的工具（GitHub、数据库、
浏览器等）桥接进内置工具系统，模型可以在对话中调用它们。

## 工作方式

- **配置目录**：`~/.zlite/mcp/` 下的每个 `.toml` 文件定义一个 MCP server，
  文件名即 server 名（一 server 一文件，文件内不重复写）。
- **后台连接**：启动时同步解析配置（快），连接在后台并行进行，不阻塞
  界面；单个 server 连接失败只降级为警告并跳过，不影响 zlite 启动与
  其他 server。
- **工具注入**：首轮对话前确保连接完成，远端工具注册进工具注册表；
  工具名为 `<server>_<tool>`（如 `github_create_issue`），来源可识别、
  跨 server 不重名。
- **权限模型**：`approve = "all"`（默认）的 server，每次工具调用前会
  弹出确认弹窗，由你决定放行（Allow）或拒绝（Deny）。

## 快速开始

1. 确认 `~/.zlite/config.toml` 中 `[mcp]` 段启用（默认开启）：

```toml
[mcp]
  enabled = true
```

2. 在 `~/.zlite/mcp/` 下创建 server 配置文件，例如 GitHub：

```bash
mkdir -p ~/.zlite/mcp
```

```toml
# ~/.zlite/mcp/github.toml
transport = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-github"]
env = { GITHUB_PERSONAL_ACCESS_TOKEN = "${GITHUB_TOKEN}" }
```

3. 重启 zlite（配置文件启动时解析）。首轮对话时 server 自动连接，
   工具即可用；`approve = "all"`（默认）时每次调用会弹确认框。

> 提示：密钥建议放在 `~/.zlite/.env`（与 config.toml 同目录），
> 通过 `${VAR}` 引用，避免明文落盘。详见 `docs/config.md`。

## 全局配置（config.toml 的 `[mcp]` 段）

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `enabled` | bool | `true` | MCP 总开关 |
| `dir` | string | `"~/.zlite/mcp"` | server 配置目录（支持 `~` 展开） |
| `max_servers` | int | `5` | 同时启用上限，超出按文件名顺序丢弃并警告（改名可调整保留谁） |
| `max_tools_per_server` | int | `20` | 单个 server 注入工具数上限，超出截断并警告 |

```toml
[mcp]
  enabled = true                  # MCP 总开关
  dir = "~/.zlite/mcp"            # server 配置目录（一 server 一个 toml 文件）
  max_servers = 5                 # 同时启用上限
  max_tools_per_server = 20       # 单 server 工具注入上限
```

## Server 配置（~/.zlite/mcp/<name>.toml）

### 字段速查

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `enabled` | bool | `true` | 单 server 开关；`false` 等价于删除文件（静默跳过） |
| `transport` | string | `"stdio"` | `stdio` / `http` / `sse` |
| `command` | string | — | **stdio 必填**，可执行命令（连接前预检，找不到报清晰错误） |
| `args` | string[] | `[]` | stdio 启动参数 |
| `env` | map | `{}` | stdio 子进程附加环境变量（合并到当前进程环境，值支持 `${VAR}` 展开） |
| `url` | string | — | **http/sse 必填**，端点地址 |
| `headers` | map | `{}` | http/sse 请求头（值支持 `${VAR}` 展开，如 `"Bearer ${TOKEN}"`） |
| `approve` | string | `"all"` | `all`（每次调用需确认，安全默认）/ `never`（信任直接执行） |
| `modes` | string[] | `["plan", "build"]` | 工具可见模式子集 |

### 文件命名

- 文件名（去 `.toml`）即 server 名，同时拼进工具名前缀，仅允许
  `字母 / 数字 / _ / -`；非法文件名该文件被跳过并警告。

### 环境变量展开

`env` 与 `headers` 的值支持 `${VAR}` 引用，且支持串内拼接
（如 `"Bearer ${TOKEN}"`）；任一变量未设置视为配置错误，该 server
跳过并警告。其他字段不做展开。

### 示例：stdio（本地进程）

```toml
# ~/.zlite/mcp/filesystem.toml
transport = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-filesystem", "/workspace"]
env = { LANG = "zh_CN.UTF-8" }          # 附加环境变量，可省
```

### 示例：http（远程端点）

```toml
# ~/.zlite/mcp/remote.toml
transport = "http"
url = "https://mcp.example.com"
headers = { Authorization = "Bearer ${TOKEN}" }   # ${VAR} 串内拼接
```

### 示例：sse

```toml
# ~/.zlite/mcp/sse.toml
transport = "sse"
url = "https://mcp.example.com/sse"
```

### 示例：信任模式 + 限定模式

```toml
# ~/.zlite/mcp/trusted.toml
transport = "stdio"
command = "my-server"
approve = "never"          # 信任该 server，调用无需确认（谨慎使用）
modes = ["build"]          # 工具仅 build 模式可见
```

## 权限与确认

- `approve = "all"`（默认）：每次调用该 server 的工具前弹确认框
  （`Allow` / `Deny`，←/→ 选择，Enter 确认，Esc = Deny），拒绝后该次
  调用不执行，模型可调整方案。
- `approve = "never"`：信任该 server，工具直接执行，无确认。
- 确认行为与内置危险命令共用同一套确认机制（TUI 居中弹窗）。

## 故障排查

配置或连接问题**不会阻断启动**，警告在启动与首轮对话时显示：

| 症状 | 原因与处理 |
|---|---|
| `mcp: <name> parse failed (skipped)` | 文件 TOML 语法错误，修正后重启 |
| `mcp: <name>: invalid transport ...` | `transport` 取值非法（stdio/http/sse），修正 |
| `mcp: <name>: transport=stdio requires command` | stdio 未填 `command` |
| `mcp: <name>: transport=http requires url` | http/sse 未填 `url` |
| `mcp: <name>: env.X: environment variable X not set` | `${VAR}` 引用的环境变量未设置，检查 `~/.zlite/.env` 或 shell 环境 |
| `mcp: server "<name>" connect failed: executable "..." not found` | stdio 的 `command` 不在 PATH，检查是否安装 |
| `mcp: server "<name>" connect failed: ...` | 连接/握手失败（网络、鉴权、协议错误），检查端点与 headers |
| `mcp: server "<name>" exposes N tools, capped at ...` | 工具数超 `max_tools_per_server`，被截断 |
| `mcp: tool name conflict "<name>" (server "..."), skipped` | 工具名与内置/其他 server 冲突，跳过该工具 |
| `mcp: N servers exceed max_servers=..., dropped: ...` | server 数超 `max_servers`，按文件名顺序丢弃后面的 |

## 注意事项

- 修改任何配置后需**重启 zlite** 生效（配置在启动时解析）。
- 退出 zlite 时自动关闭全部连接并终止 stdio 子进程（不残留孤儿进程）。
- 工具注入过多会稀释模型的工具选择，建议按需启用 server，必要时调低
  `max_servers` / `max_tools_per_server`。
- `approve = "never"` 意味着模型可在 build 模式下直接操作该 server 的
  全部能力，请只对可信 server 使用。
