# zlite 配置文件说明

配置文件默认位于 `~/.zlite/config.toml`（首次运行会提示生成模板）。
修改配置后需重启 zlite 生效。

## 密钥存放（~/.zlite/.env）

推荐把密钥写入与配置文件同目录的 `.env` 文件（`~/.zlite/.env`），
启动时自动加载（文件不存在则忽略）：

```bash
# ~/.zlite/.env
ZLITE_API_KEY=sk-xxxxxxxxxxxxxxxx
```

- `.env` 中的变量通过 `api_key = "${ZLITE_API_KEY}"` 形式引用
- 已存在的 shell 环境变量优先于 `.env`（godotenv 默认不覆盖），两者可共存
- 也可完全不写配置文件密钥：shell 里 `export ZLITE_API_KEY=...` 同样生效

## 完整配置项

```toml
# ============ 模型渠道（一期只取第一个 provider，后续扩展多个）============

[[providers]]
  name = "default"
  # type 取值规范：厂商[.协议]
  #   openai.chat      OpenAI Chat Completions（默认，兼容一切自定义端点）
  #   openai.responses OpenAI Responses API（要求端点支持 /responses）
  # 未来新增厂商直接追加枚举（如 anthropic、google、ollama），配置文件结构不变
  type = "openai.chat"

  # 自定义端点根地址（不含路径后缀，协议由 type 决定）
  base_url = "https://api.example.com/v1"

  # 支持 ${ENV} 展开；密钥建议放 ~/.zlite/.env
  api_key = "${ZLITE_API_KEY}"

  # 模型列表：可配置多个，默认使用第一个
  models = ["gpt-4o", "gpt-4o-mini"]

# ============ agent 行为 ============

[agent]
  mode = "plan"            # plan（只读：仅阅读/搜索）| build（可写：改文件/执行命令）
  auto_approve = false     # true = 信任模式，跳过危险命令确认（谨慎）
  max_steps = 16           # 单轮回答中工具调用循环的上限
  load_agents_md = true    # 自动加载项目根 AGENTS.md 注入系统提示词

# ============ shell 工具 ============

[shell]
  # build 模式下执行这些命令前缀时需要人工确认（默认值）
  confirm_commands = ["rm", "mv", "dd", "mkfs", "sudo", "chmod", "git", "git-push"]

# ============ TUI ============

[tui]
  theme = "dark"           # 界面主题（一期仅预留）

# ============ 会话 ============

[session]
  keep = 20                # 会话列表保留的最近会话数（超出的会被清理）
```

## 配置项速查表

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `providers[].name` | string | — | 渠道名（会话记录用） |
| `providers[].type` | string | `"openai.chat"` | 厂商.协议，见上方注释；未知值启动即报错 |
| `providers[].base_url` | string | — | **必填**，自定义端点根地址 |
| `providers[].api_key` | string | — | 支持 `${ENV}` 展开（来自 .env 或 shell） |
| `providers[].models` | string[] | — | **必填**，至少一个模型，默认用第一个 |
| `agent.mode` | string | `"plan"` | `plan` / `build` |
| `agent.auto_approve` | bool | `false` | 跳过危险命令确认 |
| `agent.max_steps` | int | `16` | 工具循环上限 |
| `agent.load_agents_md` | bool | `true` | 自动加载项目 AGENTS.md |
| `shell.confirm_commands` | string[] | 见模板 | 需确认的命令前缀 |
| `tui.theme` | string | `"dark"` | 主题（预留） |
| `session.keep` | int | `20` | 保留的会话数 |

## 示例：切换 Responses API

```toml
[[providers]]
  name = "openai-official"
  type = "openai.responses"      # 走 /v1/responses
  base_url = "https://api.openai.com/v1"
  api_key = "${ZLITE_API_KEY}"
  models = ["gpt-4o"]
```

> 注意：`openai.responses` 要求端点支持 `/responses`；
> 自建网关/兼容服务若只实现 Chat Completions，请保持 `openai.chat`。
