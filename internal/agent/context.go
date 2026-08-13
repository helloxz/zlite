package agent

import (
	"fmt"

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
