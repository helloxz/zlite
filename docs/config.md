# zlite 配置文件说明

配置文件默认位于 `~/.zlite/config.toml`。**首次运行（或配置不完整）时，
zlite 会直接在 TUI 界面引导配置**：依次询问 type、base_url、api_key、models，
完成后自动写入配置（api_key 存到 `~/.zlite/.env`）并热重载进入对话，无需重启。
存在任一已配置完整（name 非空且 api_key 非空）的渠道时跳过引导。

修改配置后需重启 zlite 生效。

## 密钥存放（~/.zlite/.env）

推荐把密钥写入与配置文件同目录的 `.env` 文件（`~/.zlite/.env`），
启动时自动加载（文件不存在则忽略）：

```bash
# ~/.zlite/.env
ZLITE_DEFAULT_API_KEY=sk-xxxxxxxxxxxxxxxx
```

- `.env` 中的变量通过 `api_key = "${ZLITE_DEFAULT_API_KEY}"` 形式引用
- 已存在的 shell 环境变量优先于 `.env`（godotenv 默认不覆盖），两者可共存
- 也可完全不写配置文件密钥：shell 里 `export ZLITE_DEFAULT_API_KEY=...` 同样生效

## 完整配置项

```toml
# ============ 模型渠道（可配置多个 [[providers]]）============
# 模型以 provider_name/model_name 引用（TUI /switch、默认模型、ACP 模型选项），
# 渠道名取自 name，第一个 / 前是渠道名、其余（可含 /）是模型名。

[[providers]]
  name = "default"
  # type 取值规范：厂商[.协议]
  #   openai.chat      OpenAI Chat Completions（默认，兼容一切自定义端点）
  #   openai.responses OpenAI Responses API（要求端点支持 /responses）
  #   anthropic        Anthropic Messages API（/v1/messages）
  type = "openai.chat"

  # 自定义端点根地址（不含路径后缀，协议由 type 决定）
  base_url = "https://api.example.com/v1"

  # 支持 ${ENV} 展开；密钥建议放 ~/.zlite/.env
  api_key = "${ZLITE_DEFAULT_API_KEY}"

  # 模型列表：可配置多个
  models = ["gpt-4o", "gpt-4o-mini"]

[[providers]]                    # 可继续追加渠道（如 Anthropic 官方）
  name = "claude"
  type = "anthropic"
  base_url = "https://api.anthropic.com"
  api_key = "${ANTHROPIC_API_KEY}"
  models = ["claude-sonnet-4-20250514"]

# ============ agent 行为 ============

[agent]
  mode = "plan"            # plan（只读：仅阅读/搜索）| build（可写：改文件/执行命令）
  default_model = "default/gpt-4o"  # 默认模型（provider_name/model_name）；缺省取第一个渠道第一个模型
  auto_approve = false     # true = 信任模式，跳过危险命令确认（谨慎）
  max_steps = 16           # 单轮回答中工具调用循环的上限
  load_agents_md = true    # 自动加载项目根 AGENTS.md 注入系统提示词

# ============ shell 工具 ============

[shell]
  # build 模式下执行这些命令前缀时需要人工确认（默认值）
  confirm_commands = ["rm", "mv", "dd", "mkfs", "sudo", "chmod", "git", "git-push"]
  plan_extra_commands = [] # plan 模式额外放行命令（与内置只读白名单合并、去重；如 ["python3"]）

# ============ 内置工具 ============

[tools]
  web_search = true # 启用 web_search 联网搜索工具（Tavily）；不需要联网时改为 false
```

## 配置项速查表

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `providers[].name` | string | — | 渠道名（唯一、不含 `/`；模型引用 `provider_name/model_name` 的前缀） |
| `providers[].type` | string | `"openai.chat"` | 厂商.协议，见上方注释；未知值启动即报错 |
| `providers[].base_url` | string | — | **必填**，自定义端点根地址 |
| `providers[].api_key` | string | — | 支持 `${ENV}` 展开（来自 .env 或 shell） |
| `providers[].models` | string[] | — | **必填**，至少一个模型 |
| `agent.mode` | string | `"plan"` | `plan` / `build` |
| `agent.default_model` | string | 第一个渠道第一个模型 | 默认模型引用 `provider_name/model_name`；无效引用启动即报错 |
| `agent.auto_approve` | bool | `false` | 跳过危险命令确认 |
| `agent.max_steps` | int | `16` | 工具循环上限 |
| `agent.load_agents_md` | bool | `true` | 自动加载项目 AGENTS.md |
| `shell.confirm_commands` | string[] | 见模板 | 需确认的命令前缀 |
| `shell.plan_extra_commands` | string[] | `[]` | plan 模式额外放行命令名（与内置白名单合并去重） |
| `tools.web_search` | bool | `true` | 启用 web_search 联网搜索工具（Tavily）；`false` 时不注册，模型无法联网搜索 |

## 示例：多渠道 + Anthropic + 默认模型

```toml
[[providers]]
  name = "openai-official"
  type = "openai.responses"      # 走 /v1/responses
  base_url = "https://api.openai.com/v1"
  api_key = "${ZLITE_DEFAULT_API_KEY}"
  models = ["gpt-4o"]

[[providers]]
  name = "claude"
  type = "anthropic"             # 原生 Anthropic Messages API
  base_url = "https://api.anthropic.com"
  api_key = "${ANTHROPIC_API_KEY}"
  models = ["claude-sonnet-4-20250514", "claude-haiku-4-5-20251001"]

[agent]
  default_model = "claude/claude-sonnet-4-20250514"   # 默认模型引用
```

> 注意：`openai.responses` 要求端点支持 `/responses`；
> 自建网关/兼容服务若只实现 Chat Completions，请保持 `openai.chat`。

## MCP（Model Context Protocol）

MCP server（`[mcp]` 段、`~/.zlite/mcp/` 下的一 server 一文件配置）的
完整说明见 [docs/mcp.md](mcp.md)。
