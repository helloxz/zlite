# MCP 配置与使用说明

[MCP（Model Context Protocol）](https://modelcontextprotocol.io) 是 AI 应用的
工具接入标准。zlite 通过 MCP 把外部 server 提供的工具（GitHub、数据库、
浏览器等）桥接进内置工具系统，模型可以在对话中调用它们。

## 工作方式

- **配置文件**：`~/.zlite/mcp.json` 一个文件定义全部 MCP server，使用
  官方生态通用的 `mcpServers` JSON 格式（与 Claude Code / Cursor 的
  `.mcp.json` 一致），网上教程的配置片段可直接粘贴，零转换。
- **后台连接**：启动时同步解析配置（快），连接在后台并行进行，不阻塞
  界面；单个 server 连接失败只降级为警告并跳过，不影响 zlite 启动与
  其他 server。
- **工具注入**：首轮对话前确保连接完成，远端工具注册进工具注册表；
  工具名为 `<server>_<tool>`（如 `github_create_issue`），来源可识别、
  跨 server 不重名。
- **权限模型**：默认每个工具调用前会弹出确认弹窗，由你决定放行
  （y）或拒绝（n）；`autoApprove` 白名单内的工具免确认。

## 快速开始

1. 确认 `~/.zlite/config.toml` 中 `[mcp]` 段启用（默认开启）。

2. 创建 `~/.zlite/mcp.json`，例如高德地图（npx 启动的 Node server）：

```json
{
  "mcpServers": {
    "amap-maps": {
      "command": "npx",
      "args": ["-y", "@amap/amap-maps-mcp-server"],
      "env": { "AMAP_MAPS_API_KEY": "${AMAP_MAPS_API_KEY}" }
    }
  }
}
```

> 提示：首次用 npx 启动会联网下载包（10-60 秒），可能超过连接超时；
> 建议先手动预热：`npx -y @amap/amap-maps-mcp-server`，看到下载完成后
> Ctrl+C 退出，之后再启动 zlite 即可秒连。

3. 在 `~/.zlite/.env`（与 config.toml 同目录）里配置密钥：

```bash
AMAP_MAPS_API_KEY=你的高德Web服务key
```

4. 重启 zlite（配置文件启动时解析）。首轮对话时 server 自动连接，
   工具即可用；默认每次调用会弹确认框。

## mcp.json 字段说明（官方 mcpServers 格式）

| 字段 | 类型 | 说明 |
|---|---|---|
| `type` | string | `"stdio"`（缺省）\| `"http"` \| `"sse"`：transport 选择 |
| `command` | string | stdio 启动命令（可执行文件；也支持 `"npx -y pkg url"` 整串写法，自动拆分为 command + args） |
| `args` | string[] | stdio 启动参数 |
| `env` | object | stdio 子进程附加环境变量（合并到当前进程环境） |
| `url` | string | http/sse 必填：远端端点 |
| `headers` | object | http/sse 请求头 |
| `disabled` | bool | `true` = 禁用该 server（等价于删除条目） |
| `autoApprove` | string[] | 免确认工具白名单：`["*"]` 信任全部工具；或列工具名（如 `["tavily-search"]`）；缺省 = 每次调用需确认 |

其他字段（如 `cache`、`niche` 等）被忽略，不影响使用——与其他客户端
的配置互相兼容。

**环境变量展开**：`env` / `headers` 的值支持 `${VAR}` 与 `${env:VAR}`
两种写法（含拼接，如 `"Bearer ${TOKEN}"`），变量来自 `~/.zlite/.env`
或 shell 环境（shell export 的变量优先）。变量缺失、或含 `${input:xxx}`
占位（zlite 无交互输入机制）时，该 server 跳过并警告。

**错误隔离**：单个 server 条目非法（缺 command/url、type 不认识、变量
缺失等）只跳过该条目并警告，不影响其他 server；JSON 语法错误则整个
文件失败（启动打印明确错误）。

## 示例

### 本地二进制 server（Go 实现，零运行时依赖）

```bash
go install github.com/github/github-mcp-server@latest
```

```json
{
  "mcpServers": {
    "github": {
      "command": "github-mcp-server",
      "args": ["stdio", "--toolsets", "repos,issues,pull_requests"],
      "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "${GITHUB_PAT}" }
    }
  }
}
```

### 远程 http server（GitHub 官方托管端点，零安装）

```json
{
  "mcpServers": {
    "github-remote": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": {
        "Authorization": "Bearer ${GITHUB_PAT}",
        "X-MCP-Toolsets": "repos,issues,pull_requests"
      }
    }
  }
}
```

只读变体：url 用 `https://api.githubcopilot.com/mcp/readonly`。

### 信任白名单

```json
{
  "mcpServers": {
    "tavily": {
      "type": "http",
      "url": "https://mcp.tavily.com/mcp/",
      "headers": { "Authorization": "Bearer ${TAVILY_API_KEY}" },
      "autoApprove": ["*"]
    }
  }
}
```

`["*"]` 表示信任该 server 全部工具（如纯查询类 server）；也可以只放行
部分工具：`"autoApprove": ["tavily-search"]`。

## 全局配置（config.toml 的 `[mcp]` 段）

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `enabled` | bool | `true` | MCP 总开关 |
| `file` | string | `"~/.zlite/mcp.json"` | server 配置文件路径（支持 `~` 展开） |
| `max_servers` | int | `5` | 同时启用上限，超出按 server 名排序丢弃并警告 |
| `max_tools_per_server` | int | `20` | 单个 server 注入工具数上限，超出截断并警告 |

```toml
[mcp]
  enabled = true                  # MCP 总开关
  file = "~/.zlite/mcp.json"      # server 配置（官方 mcpServers JSON 格式）
  max_servers = 5                 # 同时启用上限
  max_tools_per_server = 20       # 单 server 工具数上限
```

## 排错

- **启动时 stderr 有 `mcp: ...` 警告**：指向具体 server 的解析或连接
  问题（如 `environment variable X not set`、`executable "npx" not
  found`、`connect failed`）。按警告内容修复后重启。
- **首轮对话 TUI 出现 system 警告**：同上，连接阶段的问题会经事件流
  展示为聊天区的 system 消息。
- **工具没出现**：先看有没有上述警告；确认 `mcp.json` 语法（可用
  `python3 -m json.tool ~/.zlite/mcp.json` 校验）；确认 server 名
  符合 `[A-Za-z0-9_-]`。
- **npx server 连接超时**：首次下载包超过 10 秒请求超时，先手动预热
  （见快速开始）。
