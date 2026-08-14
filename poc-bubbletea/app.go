package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// ---- 样式：从 zlite internal/tui/render.go 原样平移 ----

const (
	ansiReset    = "\x1b[0m"
	ansiGreen    = "\x1b[32m"
	ansiYellow   = "\x1b[33m"
	ansiRed      = "\x1b[31m"
	ansiDim      = "\x1b[2m"
	ansiBarUser  = "\x1b[38;5;116m\x1b[48;5;237m"
	ansiBarZlite = "\x1b[38;5;250m\x1b[48;5;236m"
	ansiBarTool  = "\x1b[38;5;187m\x1b[48;5;236m"
	ansiFgProc   = "\x1b[38;5;214m"
	ansiFgThink  = "\x1b[38;5;111m"
	ansiFgDone   = "\x1b[38;5;114m"
)

func colorize(s, code string) string {
	if s == "" {
		return ""
	}
	return code + s + ansiReset
}

func paintLine(s, style string, w int) string {
	if s == "" {
		return ""
	}
	if w > 0 {
		if n := ansi.StringWidth(s); n < w {
			s += strings.Repeat(" ", w-n)
		}
	}
	return style + s + ansiReset
}

func paintBar(label, status, statusFg, style string, w int) string {
	s := label + status
	if s == "" {
		return ""
	}
	pad := ""
	if w > 0 {
		if n := ansi.StringWidth(s); n < w {
			pad = strings.Repeat(" ", w-n)
		}
	}
	if status == "" || statusFg == "" {
		return style + s + pad + ansiReset
	}
	return style + label + statusFg + status + pad + ansiReset
}

// mdRenderer：从 zlite 平移（流式跨片段状态保留）
type mdRenderer struct{ inCodeBlock bool }

var inlineCodeRe = regexp.MustCompile("`[^`\n]+`")

func (r *mdRenderer) Render(text string) string {
	if !strings.Contains(text, "```") && !r.inCodeBlock {
		return inlineCodeRe.ReplaceAllStringFunc(text, func(s string) string {
			return colorize(s, ansiGreen)
		})
	}
	var b strings.Builder
	rest := text
	for {
		idx := strings.Index(rest, "```")
		if idx < 0 {
			b.WriteString(r.wrap(rest))
			break
		}
		b.WriteString(r.wrap(rest[:idx]))
		rest = rest[idx+3:]
		r.inCodeBlock = !r.inCodeBlock
		if r.inCodeBlock {
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 && nl <= 32 && !strings.Contains(rest[:nl], "```") {
				rest = rest[nl+1:]
			}
		}
	}
	return b.String()
}

func (r *mdRenderer) wrap(s string) string {
	if s == "" {
		return ""
	}
	if r.inCodeBlock {
		return colorize(s, ansiGreen)
	}
	return inlineCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		return colorize(m, ansiGreen)
	})
}

// ---- model ----

type model struct {
	vp       viewport.Model
	ta       textarea.Model
	width    int
	height   int
	content  string // 聊天区完整内容（viewport 数据源）
	md       mdRenderer
	simCJK   bool
	status   string
	mode     string
	thinking string
}

func newModel(simCJK bool) model {
	ta := textarea.New()
	ta.Placeholder = "Type a message... (Enter=submit, Shift+Enter=newline, Tab=toggle mode, Ctrl+C=quit)"
	ta.ShowLineNumbers = false
	ta.Focus()
	return model{
		ta:       ta,
		md:       mdRenderer{},
		simCJK:   simCJK,
		mode:     "plan",
		thinking: "auto",
	}
}

type cjkInjectMsg struct{}

// sampleChat 生成与 zlite 一致的聊天区样例（头带 + md 着色 + 工具行 + 中文）
func sampleChat(w int) string {
	var b strings.Builder
	// 用户消息
	b.WriteString(paintLine(" You: 帮我看看这个项目的结构，并写一个示例函数", ansiBarUser, w))
	b.WriteString("\n\n")
	// 助手消息 + thinking 标记
	b.WriteString(paintBar(" Zlite: ", "[thinking...]", ansiFgThink, ansiBarZlite, w))
	b.WriteString("\n")
	b.WriteString("好的，我来分析一下。项目的核心结构如下：\n\n")
	b.WriteString("```go\n")
	b.WriteString("func main() {\n")
	b.WriteString("\tfmt.Println(\"你好，世界\")\n")
	b.WriteString("}\n")
	b.WriteString("```\n\n")
	b.WriteString("这是一个 `inline code` 示例，中文宽度 `对齐` 很关键。\n")
	// 工具行
	b.WriteString(paintLine(" [tool]", ansiBarTool, 0) + colorize("  read_file path=internal/tui/tui.go", ansiYellow))
	b.WriteString("\n")
	b.WriteString(paintLine(" [tool]", ansiBarTool, 0) + colorize("  grep pattern=\"TODO\" ... " + colorize("[ok]", ansiGreen), ansiYellow))
	return b.String()
}

func (m model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.simCJK {
		cmds = append(cmds, tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg {
			return cjkInjectMsg{}
		}))
	}
	return tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// 布局：状态边框 3 行 + 输入区 3 行，其余给聊天区；
		// 留 1 行余量避免 View() 总行数超出屏幕被终端滚动截断。
		m.vp = viewport.New(msg.Width, msg.Height-7)
		m.vp.SetContent(m.content)
		m.vp.GotoBottom()
		m.ta.SetWidth(msg.Width - 2)
		m.ta.SetHeight(3)
		return m, nil

	case cjkInjectMsg:
		// 模拟 IME commit：注入中英文混合文本
		m.ta.InsertString("你好，世界！这是 CJK 输入测试 Hello 123")
		m.status = "(simulated IME commit: 你好，世界！这是 CJK 输入测试)"
		m.ta.Focus()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "tab":
			if m.mode == "plan" {
				m.mode = "build"
			} else {
				m.mode = "plan"
			}
			m.status = "mode: " + m.mode
			return m, nil
		case "enter":
			return m.submit()
		case "shift+enter":
			m.ta.InsertString("\n")
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m model) submit() (tea.Model, tea.Cmd) {
	val := strings.TrimRight(m.ta.Value(), "\n")
	m.ta.Reset()
	m.ta.Focus()
	if strings.TrimSpace(val) == "" {
		m.status = "(empty input ignored)"
		return m, nil
	}
	// 追加用户消息 + 模拟回复（含 md 着色与中文）
	width := m.vp.Width
	if width <= 0 {
		width = 80
	}
	m.content += "\n" + paintLine(" You: "+val, ansiBarUser, width) + "\n\n"
	m.content += paintBar(" Zlite: ", "[processing...]", ansiFgProc, ansiBarZlite, width) + "\n"
	m.content += m.md.Render("收到：`"+val+"`。回复示例：```go\nprintln(\"处理完成\")\n``` 中文结尾。")
	m.vp.SetContent(m.content)
	m.vp.GotoBottom()
	m.status = "submitted: " + val
	return m, nil
}

func (m model) View() string {
	out := m.render()
	// 调试：POC_DUMP_VIEW=1 时把渲染结果写入 view.log（M0 验证用）
	if os.Getenv("POC_DUMP_VIEW") != "" {
		f, err := os.OpenFile("view.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "--- %s (w=%d h=%d) ---\n%s\n", time.Now().Format("15:04:05.000"), m.width, m.height, strings.ReplaceAll(out, "\x1b", "␛"))
			f.Close()
		}
	}
	return out
}

func (m model) render() string {
	if m.width == 0 || m.height == 0 {
		return "loading..."
	}
	var b strings.Builder
	b.WriteString(m.vp.View())
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().
		Width(m.width).
		Border(lipgloss.NormalBorder(), true, true, true, true).
		BorderForeground(lipgloss.Color("240")).
		Render(m.statusLine()))
	b.WriteString("\n")
	b.WriteString(m.ta.View())
	return b.String()
}

func (m model) statusLine() string {
	return fmt.Sprintf("[%s] poc-model | thinking: %s | %s", m.mode, m.thinking, m.status)
}
