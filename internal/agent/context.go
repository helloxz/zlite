package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/helloxz/zlite/internal/llm"
)

// defaultMaxHistoryMessages 是注入模型的历史消息条数上限（一期按条数截断）。
const defaultMaxHistoryMessages = 40

// truncateMessages 把消息序列截断为最近 max 条。
//
// 截断只切头部；同时保证配对完整性：
//   - 起始位置若落在 tool 结果上，向前扩展包含其 assistant 调用消息
//   - 起始位置之后的内容全部保留，因此不会产生孤儿 tool call
func truncateMessages(msgs []llm.Message, max int) []llm.Message {
	if len(msgs) <= max || max <= 0 {
		return msgs
	}
	start := len(msgs) - max
	for start > 0 && msgs[start].Role == llm.RoleTool {
		start--
	}
	return msgs[start:]
}

// truncationNote 生成截断说明（写入会话 meta）。
func truncationNote(total, kept int) string {
	return fmt.Sprintf("上下文截断：历史 %d 条 → 保留最近 %d 条", total, kept)
}

// maxProjectContextBytes 是 AGENTS.md 注入上下文的最大字节数（防撑爆）。
const maxProjectContextBytes = 64 * 1024

// loadProjectContext 读取项目根（cwd）的 AGENTS.md。
// 文件不存在/不可读返回空（不注入）；超过上限截断并标注。
func loadProjectContext(cwd string) string {
	data, err := os.ReadFile(filepath.Join(cwd, "AGENTS.md"))
	if err != nil {
		return ""
	}
	s := string(data)
	if len(s) > maxProjectContextBytes {
		s = s[:maxProjectContextBytes] + "\n...[truncated]"
	}
	return s
}
