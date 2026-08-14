package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/helloxz/zlite/internal/llm"
)

// defaultMaxHistoryTurns 是注入模型的历史轮次上限（按 user 消息计轮）。
// 超过该轮数才截断：丢弃最早的整轮，保留最近 maxTurns 轮。
// 按轮次而非消息条数截断：工具调用会膨胀消息条数（一轮可含多个
// tool 结果），按条数切在工具密集场景下轮数过少。
const defaultMaxHistoryTurns = 30

// maxConversationTurns 是单会话对话轮次上限：超过后拒绝继续对话。
// 过长会话会稀释早期上下文、降低模型效果，提示用户新开会话（/new）。
const maxConversationTurns = 60

// countTurns 统计消息序列的轮次数（user 消息数即轮数）。
func countTurns(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == llm.RoleUser {
			n++
		}
	}
	return n
}

// truncateMessages 把消息序列按轮次截断为最近 maxTurns 轮。
//
// 轮次以 user 消息为锚点：超限时从"倒数第 maxTurns 条 user 消息"处切割，
// 该 user 及其后所有消息（assistant/tool）构成最近 maxTurns 轮的完整内容，
// 保留区内配对天然完整，不会产生孤儿 tool call；被丢弃的头部也是完整轮次。
// 总轮数未超限时原样返回。
func truncateMessages(msgs []llm.Message, maxTurns int) []llm.Message {
	if maxTurns <= 0 {
		return msgs
	}
	// 从后往前数 user 消息：找到倒数第 maxTurns 轮的第一条消息位置。
	// 数不到（总轮数 ≤ maxTurns）则无需截断。
	start := -1
	turns := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleUser {
			continue
		}
		turns++
		if turns == maxTurns {
			start = i
			break
		}
	}
	if start < 0 {
		return msgs
	}
	return msgs[start:]
}

// truncationNote 生成截断说明（写入会话 meta，按轮次计）。
func truncationNote(total, kept int) string {
	return fmt.Sprintf("上下文截断：历史 %d 轮 → 保留最近 %d 轮", total, kept)
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
