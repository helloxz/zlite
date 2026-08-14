package session

import (
	"encoding/json"
	"os"
)

// metaVersion 是 meta 文件格式版本号，字段演进时递增兜底。
const metaVersion = 1

// Meta 是会话的列表元数据（<id>.jsonl.meta，单行 JSON）。
//
// 设计原则：jsonl 是权威数据（source of truth），meta 是其派生缓存（derived cache），
// 只放列表类操作（List/PruneEmpty/zlite --list）需要的信息，可随时删除重建：
//   - Create 时写初始 meta；Append 时增量刷新（updated_at + messages + title）
//   - open 打开会话时基于 jsonl 全量重建（一致性收敛点，顺带修复崩溃产生的落后）
//   - 删除会话时联动删除 meta（不留孤儿）
//
// 缺失/损坏的 meta 视为「无 meta」：List 跳过该会话（不兼容旧版数据）；
// 会话被打开后经 open 重建 meta，重新出现在列表。
type Meta struct {
	Version   int    `json:"version"`              // 固定 metaVersion
	ID        string `json:"id"`                   // 会话 ID（与 jsonl 首行一致）
	Title     string `json:"title,omitempty"`      // 会话标题（与 jsonl 中 title meta 一致，可能为补提取值）
	Model     string `json:"model,omitempty"`      // 会话创建时的模型
	Provider  string `json:"provider,omitempty"`   // 会话创建时的 provider
	Mode      string `json:"mode,omitempty"`       // 会话创建时的模式（plan/build）
	CreatedAt string `json:"created_at,omitempty"` // 会话创建时间（RFC3339，取自 jsonl 首行）
	UpdatedAt string `json:"updated_at,omitempty"` // 最后活跃时间（RFC3339Nano，每次 Append 刷新；List 排序键）
	Messages  int    `json:"messages"`             // TypeMessage 计数（与 List 口径一致，PruneEmpty 判空用）
}

// metaPathFor 返回 jsonl 会话文件对应的 meta 缓存路径。
// 命名 <id>.jsonl.meta：不以 .jsonl 结尾，现有 *.jsonl glob 天然匹配不到，
// 不会被 List/Continue 误当作会话文件读取。
func metaPathFor(jsonlPath string) string {
	return jsonlPath + ".meta"
}

// readMeta 读取 meta 缓存文件。
func readMeta(path string) (*Meta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// writeMeta 原子写入 meta 缓存：先写临时文件再 rename 替换，
// 保证并发读方要么看到旧 meta、要么看到新 meta，不会读到半行。
func writeMeta(path string, m *Meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
