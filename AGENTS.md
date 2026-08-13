# zlite

轻量级 CLI Coding Agent：体积小、内存占用低，面向 Linux / macOS，用于写代码场景。

## Project

- **技术栈**：Go 1.25+、gocui（TUI）、goai（LLM 流式）、viper（配置）
- **入口**：`cmd/zlite/main.go` → `cmd/zlite/app.go`（组装 config → llm → tools → session → agent → tui）
- **产物**：`make build` 生成 `bin/zlite`

## Commands

```bash
make build        # 编译（注入版本/提交/构建时间）
make test         # go test ./...
make vet          # go vet ./...
make run          # 构建并运行
bin/zlite --version
```

## Architecture

- **Agent 核心**（`internal/agent`）：对话执行者，与 UI 解耦。消费用户消息，驱动模型生成（`llm.Streamer`）、工具调度（`tools.Registry`）、权限确认（`Approver`）与会话落盘（`session.Session`），以事件流（`Events`）向外部广播进度。
- **TUI**（`internal/tui`，基于 gocui）：仅消费 agent 事件流并触发 `agent.Run`，不包含业务逻辑；通过 FIFO 队列 + 单分发 goroutine 串行化 UI 更新，防止流式增量颠倒。
- **工具系统**（`internal/tools`）：`goai.NewTool[In]` 构造，注册表附带权限元数据（`Mode` 可见性 + `NeedApprove`），按 plan/build 模式动态过滤后注入模型。
- **LLM 层**（`internal/llm`）：唯一允许依赖 goai 的包，封装 `StreamText` 为流式 `Chunk` 事件；agent/tools/tui 等包只面向本包定义的轻量类型，后期更换 AI SDK 只需改本包。
- **会话管理**（`internal/session`）：jsonl 实时落盘，支持会话恢复（`-c`）与 `/new` 切换。
- **配置**（`internal/config`）：viper 加载 `~/.zlite/config.toml`，支持 `${ENV}` 展开。

**数据流**：用户输入 → Agent.Run → 上下文截断 → 组装 System Prompt（含 AGENTS.md 实时读取）→ LLM 流式生成 → 工具调用循环（经注册表按模式过滤 + 危险命令确认）→ 结果追加到会话 → 事件广播 → TUI 渲染。

## Conventions

- 核心代码关键位置写中文注释（函数意图、复杂逻辑说明）
- 编写代码时适当考虑性能优化（避免明显低效的写法；工具输出统一截断防上下文爆炸；UI 更新串行化防并发颠倒）
- 所有用户可见文案（后端响应内容、提示、TUI 界面）一律用英文

## Notes

（待补充）
