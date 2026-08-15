package tui

// 快捷键定义（model.handleKey 使用；与 gocui 版 setup() 的键位绑定一一对应）。
//
// 行为对照（迁移约束：全部保留）：
//   - ctrl+c      退出（全局）
//   - enter       提交（输入区；textarea 默认换行，必须拦截）
//   - shift+enter 换行（新增能力，原版仅单行）
//   - tab         plan/build 模式切换（全局；不落入 textarea 插入）
//   - pgup/pgdn   聊天区翻页（弹窗打开时忽略——M2 恢复）
//   - home/end    到顶/到底（同上）
//
// Ctrl+L / Ctrl+N / Ctrl+T / Shift+Tab 依赖 picker 或对应命令，
// 在 M2 恢复为独立键位；M1 期间对应功能可用斜杠命令触发。
const (
	keyQuit       = "ctrl+c"
	keySubmit     = "enter"
	keyNewline    = "shift+enter"
	keyToggleMode = "tab"
	keyPageUp     = "pgup"
	keyPageDown   = "pgdown"
	keyTop        = "home"
	keyBottom     = "end"
)

// 弹窗与命令快捷键（Ctrl+L / Ctrl+N / Ctrl+T / Shift+Tab 与斜杠命令等效；
// M2 随 picker 弹窗一起恢复）。
const (
	keySessions   = "ctrl+l"
	keyNewChat    = "ctrl+n"
	keyThinking   = "ctrl+t"
	keySwitch     = "shift+tab"
	keyUp         = "up"
	keyDown       = "down"
	keyLeft       = "left"
	keyRight      = "right"
	keyCancel     = "esc"
)
