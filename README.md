# zlite

轻量级 CLI Coding Agent：体积小、内存占用低，面向 Linux / macOS / Windows，用于写代码场景。

> ⚠️ 开发中（阶段一进行中）。当前仅完成项目脚手架。

## 构建

要求 Go ≥ 1.25（本地已验证 1.25.7）。

```bash
make build        # 产物 bin/zlite，自动注入版本号
make test         # 单元测试
make vet          # go vet
bin/zlite --version
```

## 配置（规划）

首次运行会创建 `~/.zlite/config.toml`，示例：

```toml
[[providers]]                    # 可配置多个渠道，模型以 provider_name/model_name 引用
  name = "default"               # 渠道名（唯一、不含 /）
  type = "openai.chat"            # 厂商.协议: openai.chat | openai.responses | anthropic
  base_url = "https://api.example.com/v1"
  api_key = "${ZLITE_DEFAULT_API_KEY}"   # 支持 ${ENV} 展开，密钥不落盘
  models = ["gpt-4o"]

[agent]
  mode = "plan"                  # plan（只读）| build（可写）
  default_model = "default/gpt-4o"  # 默认模型（provider_name/model_name）；缺省取第一个渠道第一个模型
  auto_approve = false           # 信任模式（跳过写操作确认）
  max_steps = 16                 # 单轮工具循环上限

[shell]
  confirm_commands = ["rm", "mv", "dd", "mkfs", "sudo", "chmod", "git", "git-push"]
  plan_extra_commands = []       # plan 模式额外放行命令（与内置只读白名单合并去重）
```

## 功能规划

**一期（已完成）**：TUI 连续对话、流式输出、会话 jsonl 存储与 `-c` 恢复、plan/build 模式切换（Tab / `/plan` `/build`）、只读工具（read_file/grep/glob/web_fetch/run_command 只读白名单）、**build 写能力**（write_file/edit_file/delete 直接执行；run_command 全量 + 危险命令 TUI 确认）、**`/new` 新建会话**（模式重置 plan）、**`/init` 项目初始化**（AI 扫描项目生成/更新 AGENTS.md；plan 模式输出内容，build 模式写文件）、**AGENTS.md 自动加载**（每次对话读取项目根 AGENTS.md 注入系统提示词，`load_agents_md` 开关）、ASCII 边框（CJK locale 兼容）。

**二期（进行中）**：skills（SKILL.md 两级加载）、ACP 协议（`zlite acp`）、多渠道多模型（`/model`）、会话管理增强（列表选择、上下文截断细化）。

详见 [docs/plans/README.md](./docs/plans/README.md)（架构、详细设计、实施计划）与 [docs/config.md](./docs/config.md)（配置文件说明）。

## 指令和快捷键

* `/init`：初始化项目
* `/new`：新建对话，快捷键`Ctrl + N`
* 切换模式：快捷键`Tab`
* `/switch`：切换模型：快捷键`Shift + Tab`
* `/sessions`：切换对话，快捷键`Ctrl + L`
* `/thinking`：思考强度切换，快捷键`Ctrl + T`
* `/exit`：退出TUI终端，快捷键`Ctrl + C`
* 聊天区滚动：`PgUp`/`PgDn` 翻页，`Home`/`End` 跳到顶部/底部

## 许可

AGPL-3.0（见 [LICENSE](./LICENSE)）
