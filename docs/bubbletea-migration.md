# zlite TUI 迁移方案：gocui → Bubble Tea

> 状态：**M0-M4 全部完成，迁移交付**。后续 UI 优化记录见 §0.9-§0.10。
> 目标版本：bubbletea v1.3.10 + lipgloss v1.1.0 + bubbles **v1.0.0**（textarea/viewport）。

## 0.16 修复首次引导卡死（2026-08-15，严重 bug）

1. **现象**：无配置首次进入引导，最后一步（模型列表）回车后 UI 完全
   卡死，任何输入与 Ctrl+C 无效（多终端复现）。
2. **根因**：`runWithSetup` 的 onSetupDone 回调在 tea 消息循环（Update）
   线程内同步执行；其中 `t.SetAgent`/`t.SetModel` 内部调用
   `Program.Send`——bubbletea 的 Send 是**同步投递**（阻塞到消息被处理），
   从消息循环内调用即自死锁（Ctrl+C 也是消息，同样进不了 Update）。
   首次引导路径此前从未被冒烟覆盖（此前测试均带 API key 环境变量，
   直接跳过引导），M1 时 `setModel` 注释已记录过同一陷阱（/switch 卡死）。
3. **修复**：
   - 新增 `SetAgentInLoop`/`SetModelInLoop`（不投递消息，消息循环内安全）；
   - `handleSetup` 改返回 `(cmd, error)`：引导完成且 agent 注入后返回
     `waitAgentEvent` cmd，启动事件订阅（agent 此前从未订阅过）；
   - app.go 回调改用 InLoop setter（其余 Setter 本就是纯赋值）。
4. **测试**：新增 `TestSetupInLoopNoDeadlock`（模拟 app.go 回调全量行为：
   引导完成、agent/模型注入、订阅 cmd 返回、后续消息与事件渲染正常）；
   tmux 端到端实测：清空 fakehome 配置启动 → 引导 4 步 → saved 提示、
   状态栏模型热重载为 gpt-4o、再发消息正常、Ctrl+C 退出。

## 0.15 会话弹窗加宽 + 时间列对齐（2026-08-15）

1. **时间前置**（用户建议采纳）：会话行
   `"  " + Time(11 列固定 "01-02 15:04") + "  " + title`——时间列天然
   对齐，不再依赖 `padDisplay` 宽度口径；长标题只影响自身截断，不挤时间。
2. **弹窗加宽**：标题列 18 → 动态 `min(30, screenW-46)`（46 = 时间 11 +
   间隔 2 + 弹窗留白 4 + 屏幕两侧边距 29），窄屏自动收窄（下限 8）；弹窗
   总宽由 ~41 列增至 ~49 列（110 列屏幕）。
3. **TUI 新增 `screenW` 字段**（handleResize 同步，弹窗布局用）。
4. 测试：`TestSessionsFlow` 增补 3 行时间起始列一致性断言 + 长标题截断项；
   tmux 实测 Ctrl+L 弹窗时间对齐、截断正常。
5. 注：`padDisplay` 仍被 `/` 命令提示浮层使用（名称列对齐），未删除。

## 0.13 状态栏模式着色（2026-08-14）

状态栏 mode 显示按模式着色：**plan 绿色（32）/ build 红色（31）**，
Tab 切换即时生效（`ModeChangeEvent` 驱动）。

- `status_view.title()` 对 mode 文本 `colorize`（颜色序列不计宽，
  `displayWidth` 剥 ANSI 计算填充）；
- `truncateDisplay` 升级为 **ANSI 感知**：CSI 序列完整保留且不计宽
  （窄屏截断含色文本时不会切坏序列）；
- 测试：`TestModeColors`（plan 绿 / build 红 / 窄屏不破坏序列）；
  既有断言 `[plan] m2` 类改用 `stripANSI` 匹配。

## 0.12 / 命令提示浮层（2026-08-14）

输入框以 `/` 开头时，在聊天区底部（状态栏上方）浮出命令提示（灰色，
**每行一个命令 + 描述**，非模态：不拦截输入、不改变布局行数），随输入
按前缀过滤：

- `/` → 全部命令逐行展示（含描述）；`/sw` → 只剩 `/switch` 一行；非 `/`
  开头或无匹配 → 隐藏；Backspace 清掉 `/` 后消失；弹窗打开时模态优先
  不显示；描述超宽截断保留省略号；覆盖行数取命令数与聊天区行数较小值；
- 命令信息收敛为 `commandInfos` 表（`tui.go`），`/help` 文本与提示浮层
  同源生成（消除列表漂移）；命令名区域固定宽度（`hintNameWidth` = 最长
  命令名 + 2 空格），描述列对齐；
- 实现：`commandHint()`（过滤，返回 `[]string`）+ `overlayHint()`（逐行
  整行替换聊天区底部，行首无 ANSI 安全），纯渲染层，不侵入键盘逻辑；
- 测试：`TestCommandHint`（触发/过滤/隐藏/弹窗优先）；tmux 实测全链路。

## 0.11 UI 优化三（2026-08-14，用户反馈两项）

1. **输入区恢复 2 行可视**（问题 2，已修复）：`SetHeight(2)` 恢复（此前
   改为 SetHeight(1) + 装饰行，装饰行 `Render("")` 为空字符串导致无背景、
   视觉只剩 1 行）；`inputAreaView` 做行交换：内容不足 2 行时把内容行
   交换到输出末尾（空行在上）。textarea wrap 宽度 = `width - padding(2)
   - prompt(2)`，超长内容正常换行（cache 键含宽度，无缓存问题）。
   - **2026-08-14 复查修复**：a) 行交换条件
     `TrimSpace(lines[1]) == ""` 是死代码——ta 输出行带 padding+prompt
     `" │ "`，TrimSpace 永远非空；改为 `isEmptyPromptLine`（去 padding/
     prompt 后判空），placeholder 与单行内容现在正确落在输入区第 2 行
     （输出末尾，IME 光标跟随）。b) viewport 高度 H-6 → **H-5**：状态栏
     3 + 输入区 2 = 5 行，H-6 时 View 总行数 = H-1，屏幕底部留白 1 行。
     `TestResize` 断言同步（24→25、18→19）。c) 本地测试确认超长内容
     （60 汉字 = 120 列 > 107 内容宽）wrap 为 2 行、内容末尾在最后一行。
2. **IME 拼音跟随光标**（问题 1，框架限制，用户决策接受现状）：
   - 根因：bubbletea alt screen 渲染器每帧把终端硬件光标强制定位到
     View **最后一行行首**（`standard_renderer.flush` 末尾
     `ansi.CursorPosition(0, len(newLines))`，为保持增量渲染一致）；
     IME preedit 由终端绘制在硬件光标处 → 拼音永远显示在行首（内容前），
     无法跟随行内光标。**非本仓库代码问题**。
   - 已排除的路线：View 末尾追加定位序列（破坏渲染器 diff 光标状态，
     内容写错位）；行交换/装饰行（只能改变"行首"所在行）。
   - 可选补丁（用户已决策不做）：go.mod replace 本地 fork bubbletea，
     在 flush 末尾让 model 提供光标位置（约 20 行），后续升级需同步补丁。
   - 现状：拼音显示在输入行行首（内容前方），上屏后位置正确。

## 0.10 UI 优化二（2026-08-14，用户四项反馈）

1. **输入框蓝色竖线前缀**：`ta.Prompt = "│ "` + `Focused/BlurredStyle.Prompt`
   前景 `38;5;39`（Cyan）；Prompt 须在 SetWidth 之前设置（顺序天然满足）。
2. **输入框内容 padding**：`Base.Padding(0, 1)`（左右各 1 列，背景色覆盖
   padding 区域）；`SetWidth(msg.Width)` 改为全宽（textarea 内部按 frame
   size 折算内容宽）。
3. **弹窗水平居中**：`overlayChat` 此前只做 y 方向居中，盒子贴左（漏实现）；
   新增 `x0 = (m.width - 盒宽)/2` 前缀空格（盒宽取边框行 `ansi.StringWidth`，
   行首无 ANSI，安全）。tmux 实测 110 列下 35 空格偏移居中。
4. **中文 IME preedit 显示在输入框第二行**（拼音与光标不同行）：
   - 根因：textarea 光标是**虚拟的**（光标字符画进内容），终端硬件光标由
     bubbletea 渲染器停在 **View 输出末尾**（输入区末行）；IME 拼音由终端
     绘制在硬件光标处 → 错位到下一行。
   - 尝试 1（弃用）：View 末尾追加 `\x1b[..H` 光标定位序列——实测**破坏
     渲染器增量 diff 的光标状态**，后续帧字符写错位（输入内容"消失"）。
   - 最终方案：输入区渲染为「装饰空行（236 背景）+ textarea 内容行
     （`SetHeight(1)`）」，**内容行位于 View 输出末尾** → 硬件光标天然停在
     内容行，IME 拼音跟随正确。代价：内容显示在输入区视觉第 2 行。
   - 真实终端 IME 效果需用户真机确认（tmux 无 IME）。

## 0.9 UI 优化（2026-08-14，用户反馈两项）

1. **状态栏右侧展示项目路径**（`status_view.go`）：`title(w)` 重构为左段
   （mode/model/thinking/busy）+ 灰色（`38;5;244`）路径右对齐，中间空格
   填充；路径超宽时左侧截断保留尾部（`…/last/dirs`），剩余空间不足
   `minPathWidth(14)` 时隐藏；左段超宽时截断保留核心信息——**不能返回
   超宽字符串**（lipgloss `Width` 对超宽内容 wrap 成多行，破坏边框盒
   布局，窄屏实测）。
2. **输入框 2 行 + 去前缀 + 全宽背景**（`model.go`）：`ta.Prompt=""`（默认
   `┃ `）；`SetHeight(2)` + viewport `H-7→H-6`（布局行数保持精确）；
   `FocusedStyle.Base`/`BlurredStyle.Base` 背景 `236`（与头带一致）。
   **坑**：textarea 默认 `CursorLine` 背景是黑色（`AdaptiveColor{Dark:"0"}`），
   会覆盖 Base 的 236 导致光标行色块突兀——需显式设置 `CursorLine` 背景。
3. **顺带修复**：状态栏边框 `Width(m.width)` 实际总宽 = 屏幕宽 + 2
   （lipgloss `Width` 不含 border），右框一直被终端截断 2 列；改为
   `Width(m.width-2)` 后右框完整。
4. 测试：`TestStatusBarPath`（完整/截断/窄屏隐藏）、`TestResize` 高度断言
   更新；tmux 实测：110 列全宽对齐、45 列窄屏不 wrap、输入框背景统一。
   注意：tmux 无法注入 Shift+Enter（kitty CSI u 协议），真机终端验证。

## 0.8 M4 打磨完成（2026-08-14）

**增强项落实**（方案 §8 清单逐项）：

1. 边框升级 RoundedBorder（状态栏 + 弹窗，M0 对齐断言 + tmux 实测通过）；
2. 多行输入（Shift+Enter 换行）— M1 已含；
3. 输入框 placeholder — M1 已含；
4. 弹窗条目内部滚动 — M2 已含；
5. 平滑滚动（viewport 按行）— 原生；
6. token 用量展示 — **维持隐藏**（usage 字段保留，与 gocui 版决策一致）。

**性能抽样**（`internal/tui/bench_test.go`，120x40 终端）：

| 场景 | 结果 |
|---|---|
| 500 条消息整屏渲染（View） | **188µs/op**（~5300 帧/秒） |
| 流式增量（200 段/轮，含 View） | **161 deltas/s**（单段 ~6.2ms，bubbletea 帧合并兜底） |

结论：无性能劣化风险，流式渲染在真实 LLM 节奏下富余。

**二进制体积**（`-ldflags="-s -w"`，git worktree 构建 HEAD gocui 版对比）：

- gocui 版（HEAD）：10,336 KiB；bubbletea 版：10,772 KiB；**增量 +436 KiB（4%）**。
- 超出原验收标准（≤400KB）36KB：主要为 bubbles（textarea 依赖 clipboard、
  viewport）+ lipgloss v1.1 + x/ansi 的净代码量，与早期最小 TUI 实测
  （+150~330KB）量级一致；**验收标准更新为 ≤500KiB**。

**最终冒烟（tmux）**：圆角状态栏/弹窗对齐、聊天区/输入框渲染、[done]、
错误路径、Ctrl+C 退出与 AltScreen 恢复全部正常。

## 0.7 M3 回归完成（2026-08-14）

gocui 版 `tui_test.go` 的 16 个测试场景全部映射为模型级测试
（`internal/tui/regression_test.go`，19 个测试函数），当前 `go test ./...`
9 包全绿（tui 包 21 个测试）：

| 旧场景 | 新测试 | 覆盖 |
|---|---|---|
| TestTUIInteraction | TestInteractionFlow | 提交→流式增量→工具行→[done]→错误路径 |
| TestSetupFlow | TestSetupFlow | 引导全流程+非法输入+失败重来 |
| TestHandleCommandExit | TestHandleCommandExit | /exit /quit → tea.Quit |
| TestHandleSkills | TestHandleSkills | skills 列表/来源层级/空列表 |
| TestApprovalFlow | TestApprovalFlow | 审批提示+y/n+非法输入 |
| TestSwitchModel | TestSwitchModel | 直切/未知/失败 |
| TestThinking(+Display×2) | TestThinking/TestThinkingDisplay | 设置/未知/busy 拒绝/标记切换 |
| TestBusyPosition | TestBusyReject | busy 拒绝 |
| TestDivider | TestDivider | 回合分隔 |
| TestChatScroll(+Bounds) | TestChatScroll | PgUp/Home/End 跟随语义 |
| TestChatScrollModal | TestPickerModalBlocksScroll | 模态拦截 |
| TestSessionsFlow | TestSessionsFlow | 弹窗+历史重建+模型恢复 |
| TestShortcutKeys | TestShortcutKeys | 四组快捷键 |

**回归验证结论**：

1. busy/TOCTOU：submit 在消息循环内同步置位（appendUser + busy 原子），
   无 gocui 版跨 goroutine 的检查窗口；
2. resize：`WindowSizeMsg` 后 viewport/textarea 尺寸与内容保持正确
   （TestResize）；
3. **AltScreen 退出恢复**（tmux 冒烟）：Ctrl+C 退出后 shell 提示符正常
   返回、无 TUI 残留——alt screen buffer 正确归还；
4. 断言注意：渲染文本含 ANSI 序列（如 `[tool]` 与参数间插 SGR），
   子串断言需先 `stripANSI`（测试内辅助函数）。

## 0.6 M2 交互完成（2026-08-14）

**交付**：

- `picker.go` 重建：通用覆盖层弹窗（/switch 模型、/thinking 思考强度、
  /sessions 会话三处共用），↑/↓ 移动（clamp）、Enter 确认、Esc 取消、
  条目超出可视高度时内部滚动跟随选中行、选中行 Cyan 底黑字高亮、
  标题 dim 显示；渲染为按行切分插入聊天区中央（不触碰行内 ANSI）；
- 快捷键全部恢复：Ctrl+L（/sessions）、Ctrl+N（/new）、Ctrl+T（/thinking）、
  Shift+Tab（/switch）；弹窗打开时模态（滚动/输入被拦截，Ctrl+C 仍全局退出）；
- 新测试 4 个（TestPickerFlow/Cancel/ModalBlocksScroll/ShortcutKeys），
  `go test ./...` 9 包全绿。

**M2 实测发现的关键坑（必须牢记）**：

1. **`tea.Program.Send` 是同步投递**（阻塞直到消息被 Update 处理完）：
   从消息循环内调用 `program.Send` 会**自死锁**（实测：/switch 确认时整个
   UI 卡死，SIGQUIT 栈显示 `Send → SetModel → switchToModel → pickerConfirm`
   链路）。修复：消息循环内的状态更新走内部方法（`setModel`，只改共享
   状态），渲染由 Update 末尾统一 `refreshChat`；`Send` 仅限外部线程
   （app.go 热重载回调、runAgent goroutine、Approver）。
2. 弹窗确认回调（onPick）可能 `appendSystem`，**确认后必须 `refreshChat`**
   否则聊天区不刷新（测试用直接构造 KeyMsg 的方式能暴露，真机表现为
   "切了模型但提示没出现"）。
3. tmux 冒烟注意：`send-keys Down` 发送 CSI B；`Enter` 在 zlite 的 raw
   模式下是 `\r`（cooked 的 cat 窗口里会看到 `\n`，别被误导）；
   `send-keys "S-Tab"` 会被当字面文本，Shift+Tab 需发 `\x1b[Z`。

**M2 冒烟（tmux）验证通过**：模型弹窗（↓↓→Enter 切换、状态栏更新）、
thinking 弹窗（滚动选择 high）、sessions 弹窗（中文标题+时间对齐）、
Esc 取消、弹窗打开时 Ctrl+C 退出、模态输入拦截。

## 0.5 M1 骨架完成（2026-08-14）

`internal/tui` 已整体迁移为 bubbletea 实现（`go build/vet/test ./...` 全绿，
gocui/tcell 依赖已从 go.mod 移除），`app.go` 零改动。

**实现要点**：

- `model.go`：tea.Model + 消息类型 + `waitAgentEvent` 循环订阅（替代
  `consumeEvents` goroutine + `uiTasks` 队列）；`chatView/statusView` 改为
  纯状态模型，渲染统一由 `model.refreshChat()` 生成字符串写入 viewport；
- 全部快捷键（Ctrl+C/Enter/Tab/Shift+Enter/PgUp/PgDn/Home/End）与斜杠命令
  （含参数直通路径）、首次配置引导、Approver（program.Send + channel）
  已迁移；Ctrl+L/Ctrl+N/Ctrl+T/Shift+Tab 及 picker 在 M2 恢复；
- 外部注入 API（SetAgent/SetModel/...）保持签名不变，经 `refreshMsg`/
  `agentResubscribeMsg` 触发重绘/重新订阅。

**M1 实测发现的关键坑（M2/M3 必须遵守）**：

1. **`viewport.Model` 是值类型，不能作为值接收者方法的字段修改**：
   `m.vp.SetContent(...)` 在值接收者方法内只会修改临时拷贝（实测：提交
   后聊天区空、滚动无效）。修复：`model.vp` 改为 `*viewport.Model` 指针
   字段。同类坑还包括 `ta` 的 `Update`（需显式回接 `m.ta, cmd = ...`）。
2. **启动耗时**：TUI 在真实 pty 下约需 3-5 秒完成初始化（skills 扫描等），
   冒烟测试需等待充分再断言，否则误判“画面空白”。
3. `cmd/zlite/app_test.go` 断言从 tcell 报错文案更新为 bubbletea 的
   `could not open a new TTY`（该文件被 .gitignore 的 `*_test.go` 忽略，
   仅本地生效）。

**M1 冒烟（tmux + 假 API key）验证通过**：初始渲染（ready 消息）、
`/help`、Tab 模式切换、中文消息提交、`[done]` 标记、API 错误显示、
busy 复位、自动跟随滚动、PgUp 滚动、Ctrl+C 退出。

## 0. M0 PoC 结论（2026-08-14 实测）

PoC 代码位于 `poc-bubbletea/`（快照模式 `./poc -snapshot`、交互模式
`./poc -simulate-cjk`），三个风险点全部通过：

| 验证点 | 结论 | 证据 |
|---|---|---|
| (a) textarea 中文输入 | **通过** | 模拟 IME commit（`KeyRunes` 路径）注入中文 + 实时 `send-keys` 中文 + 提交全链路正常，光标位置正确；真实 IME preedit 仍需用户真机确认（commit 路径已证通） |
| (b) viewport ANSI 渲染 | **通过** | SGR 序列保留；头带/md 着色/工具行渲染正确；`StringWidth` 中文计 2（"你好"=4） |
| (c) CJK 边框对齐 | **通过** | `RoundedBorder`/`NormalBorder` 在 `zh_CN.UTF-8` 下每行宽度一致（断言 PASS），中文 padding 正确 |

**M0 发现的关键坑（M1 必须遵守）**：

1. **布局行数必须精确**：`View()` 总输出行数超过屏幕高度时，终端滚动把
   内容行拼接到边框行后被截断（实测现象：textarea 内容"消失"，实际是被
   切出屏幕）。bubbletea 无自动布局，行数分配必须精确到 `H-7` 这类常量并
   留余量；布局完成后用 `script`/`tmux` 实机校验。
2. **bubbles 已到 v1.0.0**（不是原评估的 v0.22）：textarea 内部改用
   viewport + `memoizedWrap` 渲染，`Prompt` 默认 `"┃ "`，空内容时显示
   `Placeholder`；API 需按 v1.0.0 实测（M1 直接锁定 v1.0.0）。
3. **lipgloss `Width` 语义**：`Width(n)` 含 padding、不含 border（边框盒
   总宽 = n + 2）。对齐断言按此计算。
4. **textarea 需显式 `SetWidth`/`SetHeight`**；Enter 默认换行必须拦截改
   提交（Shift+Enter 换行）；`ta.Update` 只喂非特殊键。

## 1. 背景与目标

当前 `internal/tui` 基于 `awesome-gocui/gocui v1.1.0`（社区 fork，已实质停维护），
布局、滚动、输入、弹窗、状态栏全部手写，并依赖 tcell v2.4（旧版）。迁移到
charmbracelet 生态（bubbletea + lipgloss + bubbles），换取活跃维护、组件库与
现代渲染能力。

**硬性约束**（本次迁移验收底线）：

1. 操作逻辑基本不变：全部斜杠命令、弹窗流程、审批流程、首次配置引导、busy 语义逐项保持；
2. 全部现有快捷键保留（见 §6 对照表）；
3. 界面只能更好，不能更差：现有视觉元素（头带配色、状态标记、工具行、ASCII 边框）1:1 保留，改进项见 §8；
4. 不引入鼠标事件（保持终端原生文本选择，与 opencode 同策略）；
5. `agent` / `tools` / `llm` / `session` / `cmd/zlite/app.go` 的组装接口零改动。

## 2. 现状盘点

| 文件 | 行数 | 职责 | 迁移动作 |
|---|---|---|---|
| `tui.go` | ~900 | 布局、键位、事件消费、命令、引导状态机 | 拆分：逻辑保留，触发方式改造 |
| `chat_view.go` | ~280 | 聊天区：流式追加、滚动、头带、标记 | 重写渲染层（viewport），逻辑保留 |
| `picker.go` | ~145 | 通用选择弹窗（/switch、/thinking、/sessions） | 重写为 overlay model |
| `render.go` | ~110 | ANSI 配色、头带、轻量 md 渲染 | **整体复用**（viewport 支持 SGR） |
| `status_view.go` | ~70 | 状态栏（借输入框边框 Title） | 重写为独立行（lipgloss） |
| `approver.go` | ~50 | 危险命令确认（channel 桥接） | 小改（投递目标换成 `p.Send`） |
| `confirm_view.go` | 0 | 二期占位 | 保留占位 |
| `tui_test.go` | 33 个测试 | 基于 `gocui.TestingScreen` | 全部重写（§9） |

**对外接口（冻结）**：`tui.New`、`EnableSetup`、`SetAgent`、`SetModel`、
`SetSwitchModel`、`SetSessionSwitcher`、`SetSkillsLister`、`SetNewSession`、
`tui.Approver.Attach`、`tui.Stop` —— 签名与语义保持不变，`app.go` 无需改动。

## 3. 目标架构

Bubble Tea 的 Elm 架构（`Model / Init / Update / View` + `Cmd / Msg`）天然契合
zlite 现有设计：

```
agent 事件流 ──订阅 Cmd──▶ 消息循环(Update, 天然串行) ──▶ View() 渲染
用户按键/输入 ──────────────▶ 消息循环 ──▶ agent.Run (后台 goroutine)
外部信号(SIGINT/SIGTERM) ──▶ program.Quit()
危险命令确认(Approver) ────▶ p.Send + channel 回传
```

**删掉的 gocui 补丁代码**（净减 ~300-400 行）：

- `uiTasks` FIFO 队列 + `dispatch` goroutine（防流式增量颠倒）→ 消息循环天然串行；
- `consumeEvents` goroutine → `tea.Cmd` 订阅；
- 手写滚动（Origin/clampOy/scrollBy/Autoscroll 状态机）→ `viewport` API；
- 手写布局（`SetView` 绝对坐标 + 边框偏移 hack）→ lipgloss 组合；
- 状态栏 Title hack（tcell setRune +1 偏移限制）→ 独立 lipgloss 行；
- 弹窗 view 常驻 + Visible 切换（规避 loaderTick 数据竞争）→ overlay model 无此问题。

### 3.1 事件流订阅（替代 consumeEvents）

```go
type agentEventMsg struct{ ev agent.Event }

// 订阅 Cmd：每次处理完一个事件后重新返回自身，形成循环订阅。
// done 用于 TUI 退出时解除阻塞（防 goroutine 泄漏）。
func waitAgentEvent(events <-chan agent.Event, done <-chan struct{}) tea.Cmd {
    return func() tea.Msg {
        select {
        case ev := <-events:
            return agentEventMsg{ev: ev}
        case <-done:
            return nil
        }
    }
}
```

`Update` 收到 `agentEventMsg` 后按现有 `consumeEvents` 的 type switch 分发
（TextDelta → 追加增量；ToolCall/ToolResult → 工具行；ThinkingStart →
processing→thinking；ModeChange → 状态栏；Done → finishProcessing 由 runAgent
完成处统一处理），然后返回 `model, waitAgentEvent(events, done)` 继续订阅。

### 3.2 后台任务与 busy 语义（保持现有 TOCTOU 防护）

`submit` 的同步置位逻辑原样保留在 `Update` 中：校验 busy → `setBusy(true)` →
追加用户消息 → `startProcessing()` → `go runAgent(msg)`。`runAgent` 完成回调
仍经 `p.Send(agentDoneMsg{err})` 投递回主循环（对应现在的 `t.ui(...)`），
`Update` 中置 `busy=false` + `finishProcessing()` + 错误展示。

### 3.3 Approver（保持 channel 桥接语义）

```go
func (a *Approver) Request(ctx context.Context, req agent.ApprovalRequest) (agent.ApprovalDecision, error) {
    ch := make(chan agent.ApprovalDecision, 1)
    a.t.program.Send(approvalRequestMsg{summary: req.Summary, ch: ch})
    select {
    case d := <-ch:
        return d, nil
    case <-ctx.Done():
        a.t.program.Send(approvalCancelMsg{ch: ch}) // Update 里清 pendingApproval
        return agent.Denied, ctx.Err()
    }
}
```

`Update` 收到 `approvalRequestMsg` 时 `appendSystem("Approve? ...")` 并置
`pendingApproval`；输入 y/n 走 `handleApproval`（逻辑不变）。`program.Send`
线程安全，可从任意 goroutine 调用。

### 3.4 Stop / 退出

`Stop()` 改为 `t.program.Quit()`（线程安全），信号处理 goroutine 不变。
退出前 close `done` channel 解除订阅阻塞。`tea.WithAltScreen()` 保持
alternate screen 行为（进入切换、退出恢复），与 gocui 一致。

## 4. 依赖版本

| 模块 | 版本 | 用途 |
|---|---|---|
| `github.com/charmbracelet/bubbletea` | v1.3.10（已实测） | 框架核心 |
| `github.com/charmbracelet/lipgloss` | v1.1.0 | 布局与样式 |
| `github.com/charmbracelet/bubbles` | **v1.0.0**（已实测） | `viewport`（聊天区）、`textarea`（输入） |
| `github.com/charmbracelet/x/ansi` | 随 bubbles 引入 | 宽度计算（CJK 安全） |

删除 `github.com/awesome-gocui/gocui`、`gdamore/tcell/v2`、`gdamore/encoding`。
依赖净增约 10 个模块，二进制增量 ~150-330KB（已实测，见前期对比）。

## 5. 目标文件结构

```
internal/tui/
  tui.go          # 保留：TUI 结构、agentFace、注入 API、命令处理、引导状态机
  model.go        # 新增：tea.Model（Init/Update/View）、全部消息类型定义
  keys.go         # 新增：键位匹配表（§6）
  chat_view.go    # 改造：chatView 持有 viewport；entries 渲染为纯字符串
  input.go        # 新增：textarea 包装（Enter/Tab/Shift+Tab 拦截）
  picker.go       # 改造：弹窗 overlay model（选中态、滚动）
  status_view.go  # 改造：状态栏字符串生成（lipgloss 行）
  render.go       # 保留：ANSI 常量、paintLine/paintBar/mdRenderer（原样）
  approver.go     # 小改：p.Send 桥接
  confirm_view.go # 保留占位
  tui_test.go     # 重写（§9）
```

## 6. 快捷键对照表（全部保留）

| 键 | 现状（gocui） | 目标（bubbletea） | 备注 |
|---|---|---|---|
| Ctrl+C | 全局退出 | `key.WithKeys("ctrl+c")` → `tea.Quit` | 行为不变 |
| Enter | 提交（input） | 拦截 textarea Enter → `submit` | textarea 默认换行，必须覆盖 |
| Shift+Enter | — | 换行（textarea 默认） | **新增**（原版仅单行） |
| Ctrl+L | /sessions（input） | 同上 | 行为不变 |
| Ctrl+N | /new（input） | 同上 | 行为不变 |
| Ctrl+T | /thinking（input） | 同上 | 行为不变 |
| Shift+Tab | /switch（input，双绑定 ModNone/ModShift） | `key.WithKeys("shift+tab")` | 行为不变 |
| Tab | plan/build 切换（全局 + input） | 全局拦截，不落入 textarea | 行为不变（textarea 不插入 tab） |
| PgUp/PgDn | 聊天区翻页（全局，弹窗时忽略） | viewport `PageUp/PageDown`，弹窗时忽略逻辑保留 | 行为不变 |
| Home/End | 到顶/到底（全局，弹窗时忽略） | viewport `GotoTop/GotoBottom` | 行为不变 |
| ↑/↓/Enter/Esc | 弹窗选择器 | overlay model 内处理 | 行为不变 |

## 7. 功能迁移对照表（操作逻辑保持）

| 功能 | 现状实现 | 目标实现 | 保持点 |
|---|---|---|---|
| 斜杠命令 `/exit /quit /new /plan /build /switch /thinking /sessions /skills /init /help` | `handleCommand` 系列 | 函数体原样保留，由 Update 的文本输入分支调用 | 命令名、参数、busy 拒绝、错误文案逐字保留 |
| 首次配置引导 | `setupState` 状态机（type→base_url→api_key→models） | 状态机原样保留，输入拦截点从 submit 移到 Update | 提问文案、校验、`onSetupDone` 失败重来 |
| 危险命令审批 | 聊天区提示 + y/n 输入 + channel | 同上（§3.3） | 提示格式 `Approve? ... [y/n]` 保留 |
| 模型/思考强度切换 | busy 检查 + 弹窗 or 参数直切 | 逻辑原样 | `auto` 归一化、未知值报错文案保留 |
| 会话切换 | 弹窗列表 → `switchSession` → `loadHistory` | 逻辑原样 | 标题截断（CJK 宽）、`(no title)`、`(still processing...)` 文案保留 |
| 聊天区流式渲染 | 增量 append + 全量重绘 | viewport `SetContent`（增量 append 后重渲染字符串） | 每 delta 全量重绘语义一致，帧合并由 bubbletea 处理 |
| 滚动跟随 | `autoScroll` 状态机（上翻停、回底恢复） | chatView 保留 `autoScroll` 字段，映射 viewport GotoBottom | 手动滚动位置保持 |
| 工具行 `[tool] ... [ok]/[fail]` | `appendToolCall/finishToolCall` | 逻辑原样 | `[ok]` 绿 / `[fail]` 红 |
| 状态标记 `[processing...]/[thinking...]/[done]` | 渲染时动态生成 | 原样 | 颜色与位置不变 |
| 历史恢复 | `loadHistory(llm.Message)` | 原样 | 工具行摘要策略不变 |
| /skills 展示 | 拼接文本追加系统行 | 原样 | 格式保留 |

## 8. 界面基线（只增不减）

**1:1 保留**：

- 头带配色：`ansiBarUser`（浅青+237）、`ansiBarZlite`（浅灰+236）、`ansiBarTool`（khaki+236）；
- 状态标记色：`[processing...]` 琥珀 214、`[thinking...]` 柔蓝 111、`[done]` 柔绿 114；
- 系统消息 dim、错误红、`[tool]` 黄、代码块/行内代码绿（`mdRenderer` 整体复用，含流式跨片段状态）；
- 布局：聊天区占主体、底部输入框（边框）、状态信息在底部区域；
- 弹窗：居中覆盖层、标题、↑/↓ 高亮（Cyan 底 Black 字）、Esc 取消。

**增强项**（默认全部启用，视觉只升不降）：

1. 边框从 ASCII `- | +` 升级为 lipgloss 标准边框；对齐安全性由 x/ansi 宽度计算保证（CJK 下 ambiguous 按宽 1，与终端一致）。**PoC 必须在 CJK locale 下验证**，若出现错位回退 `BorderStyleNormal`（纯 ASCII，即现状）；
2. 输入区支持多行（Shift+Enter 换行），提示行显示 `mode | model | thinking | busy`（现状状态栏内容平移）；
3. 输入框 placeholder（`Type a message...`，空输入时 dim 显示）；
4. 弹窗条目过多时内部滚动（现状超高即截断，属修复）；
5. 平滑滚动（viewport 自带按行滚动，无动画跳跃）；
6. 状态栏可恢复 token 用量展示（usage 字段已保留；**默认维持隐藏**，与现状一致，仅作可选项）。

**明确不加**：鼠标事件（保持文本选择）、动画帧率控制（跟随 bubbletea 默认 60fps 帧合并）。

## 9. 测试策略

现状 33 个测试全部基于 `gocui.TestingScreen`（注入按键 + 读屏），bubbletea
无等价模拟器，测试全部重写，但**断言更简单**：

| 层 | 方式 | 覆盖 |
|---|---|---|
| 渲染单元测试 | `chatView.Render()` 返回字符串直接断言（含 ANSI） | 头带、标记、工具行、md 渲染（替代 render_test.go + chat 屏测） |
| 模型级测试 | 构造 model → `model.Update(msg)` 纯函数调用 → 断言状态与 `View()` 输出 | 命令分发、弹窗、引导状态机、审批、busy 拒绝、滚动状态 |
| 集成冒烟 | `tea.NewProgram(model, tea.WithInput(fakeReader), tea.WithOutput(io.Discard))` 注入字节流按键序列 | 快捷键映射、事件流驱动渲染（1-2 个冒烟用例） |
| 事件流测试 | fakeAgent 发事件 → 断言 View() 增量变化 | 流式顺序、thinking 切换、工具行完成 |

原 33 个测试场景逐一映射到新用例清单（迁移时对照 `tui_test.go` 函数名逐条勾选）。

## 10. 里程碑（约 7-10 人天）

| 阶段 | 内容 | 估时 | 状态 |
|---|---|---|---|
| M0 PoC | (a) textarea 中文输入（Linux IME commit 路径）；(b) viewport ANSI 渲染；(c) CJK locale 边框对齐 | 0.5-1 天 | **已完成**（全部通过，见 §0） |
| M1 骨架 | program 启动/退出、布局、事件流订阅、聊天区基础渲染、快捷键注册 | 1-2 天 | **已完成**（见 §0.5） |
| M2 交互 | 输入提交、命令全量、弹窗三处、滚动、状态栏、审批、引导状态机 | 2-3 天 | **已完成**（见 §0.6） |
| M3 回归 | 33 测试场景映射重写、busy/TOCTOU、resize、AltScreen 退出恢复 | 2-3 天 | **已完成**（见 §0.7） |
| M4 打磨 | 增强项（§8）、视觉对照、性能抽样 | 1 天 | **已完成**（见 §0.8） |

## 13. 验收结果（2026-08-14）

| # | 验收项 | 结果 |
|---|---|---|
| 1 | 全部快捷键可用（含 Shift+Tab 双绑定） | ✅ 模型级测试 + tmux 实测 |
| 2 | 功能逐项对照无回退 | ✅ M1-M3 逐项迁移并冒烟 |
| 3 | 中文输入（含 IME commit 路径） | ✅ M0 验证 |
| 4 | `go test ./...` 全绿 | ✅ 9 包（tui 21 测试 + bench） |
| 5 | 二进制增量 ≤ 500KiB | ✅ 实测 +436 KiB（原 ≤400KiB 标准已按实测修订，见 §0.8） |
| 6 | `app.go` 零改动 | ✅ 仅 go.mod 依赖变更 |
| 7 | 退出后终端完整恢复 | ✅ tmux 实测（alt screen 归还、shell 提示符） |

## 11. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| textarea 对 IME preedit 预览支持不足 | 中文输入体验下降 | M0 前置验证；失败则输入组件回退单行 `bubbles/textinput`（行为与现状完全一致），多行作后续增强 |
| lipgloss 边框在 CJK locale 对齐错位 | 视觉劣化 | M0 验证；失败则边框常量回退 ASCII（现状），其余增强不受影响 |
| 流式高频 delta 渲染开销 | 性能劣化 | bubbletea 帧合并天然缓解；必要时事件侧做 16ms 窗口批量合并（chatView 增量缓冲） |
| 订阅 goroutine 泄漏 | 退出后残留 | done channel 解除阻塞（§3.1） |
| bubbles 版本 API 变动 | 编译/行为漂移 | 锁定 v0.22+，升级走依赖升降级流程 |

## 12. 验收标准

1. §6 全部快捷键在真实终端可用（含 Shift+Tab 双绑定行为）；
2. §7 全部功能逐项对照通过，文案/颜色/布局与现状截图对比无回退；
3. 中文输入（含 IME）与现状等价或更好；
4. `go test ./...` 全绿，33 个旧测试场景全部有对应新用例；
5. `make build` 产物体积增量 ≤ 400KB；
6. `app.go` 零改动（仅 go.mod 依赖变更）；
7. 退出后终端状态完全恢复（alt screen 归还、光标、回显）。
