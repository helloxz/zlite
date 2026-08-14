package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/helloxz/zlite/internal/config"
	"github.com/helloxz/zlite/internal/version"
)

// ErrNoSession 表示当前目录没有可继续的会话。
var ErrNoSession = errors.New("no session found")

// Info 是会话列表中的一项。
type Info struct {
	ID        string
	Path      string
	Model     string
	Provider  string
	Mode      string
	Title     string
	CreatedAt time.Time // 最后活跃时间（取自 meta.UpdatedAt；List 排序键，兼容旧 ModTime 语义）
	Messages  int
}

// Manager 管理会话文件的创建、继续与列表。
type Manager struct {
	sessionsDir string // ~/.zlite/sessions
}

// NewManager 创建管理器。
func NewManager(sessionsDir string) *Manager {
	return &Manager{sessionsDir: sessionsDir}
}

// hashCwd 把工作目录哈希为目录名（SHA-256 前 12 位）。
func hashCwd(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return hex.EncodeToString(sum[:])[:12]
}

// dirFor 返回 cwd 对应的会话目录。
func (m *Manager) dirFor(cwd string) string {
	return filepath.Join(m.sessionsDir, hashCwd(cwd))
}

// Create 新建会话并写入首行元信息。
func (m *Manager) Create(cwd string, p *config.Provider, mode string) (*Session, error) {
	dir := m.dirFor(cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建会话目录失败: %w", err)
	}

	now := time.Now()
	id := newSessionID(now)
	path := filepath.Join(dir, id+".jsonl")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("创建会话文件失败: %w", err)
	}

	s := &Session{
		ID: id, Path: path, file: f,
		Mode: mode, Model: p.Models[0], Provider: p.Name,
		CreatedAt: now.Format(time.RFC3339), metaPath: metaPathFor(path),
	}
	if s.Provider == "" {
		s.Provider = "default"
	}

	head := Record{
		Type: TypeSession, ID: id,
		Cwd: cwd, CreatedAt: now.Format(time.RFC3339),
		Model: p.Models[0], Provider: s.Provider, Mode: mode,
		Version: version.String(), Ts: now.Format(time.RFC3339),
	}
	line, err := head.encode()
	if err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return nil, fmt.Errorf("写入会话元信息失败: %w", err)
	}
	// 写初始 meta 缓存（列表用；缓存语义，失败静默，打开时会重建）。
	s.syncMeta(now.Format(time.RFC3339Nano))
	return s, nil
}

// Continue 打开 cwd 下最新的会话（按文件修改时间）。
func (m *Manager) Continue(cwd string) (*Session, error) {
	dir := m.dirFor(cwd)
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(matches) == 0 {
		return nil, ErrNoSession
	}

	sort.Slice(matches, func(i, j int) bool {
		mi, _ := os.Stat(matches[i])
		mj, _ := os.Stat(matches[j])
		return mi.ModTime().After(mj.ModTime())
	})
	return m.open(matches[0])
}

// Open 按会话 ID 打开 cwd 下的会话（/sessions 切换用）。
// 会话不存在时返回 ErrNoSession。
func (m *Manager) Open(cwd, id string) (*Session, error) {
	path := m.SessionPath(cwd, id)
	if _, err := os.Stat(path); err != nil {
		return nil, ErrNoSession
	}
	return m.open(path)
}

// SessionPath 返回 cwd 下指定 id 会话的 jsonl 路径（不访问磁盘，可能不存在）。
// 供调用方按 ID 定位会话文件（如删除时拼路径），与 Open 的定位口径一致。
func (m *Manager) SessionPath(cwd, id string) string {
	return filepath.Join(m.dirFor(cwd), id+".jsonl")
}

// PruneEmpty 删除最近 limit 条会话中没有任何对话记录（message）的空会话
// （创建后未对话即退出的残留），返回删除数量。
// skipID 指定跳过不删的会话 ID（如当前打开的会话，文件句柄仍在使用）。
// 判定口径与 List 一致：仅数 TypeMessage，meta/tool 记录不算对话内容。
func (m *Manager) PruneEmpty(cwd string, limit int, skipID string) (int, error) {
	infos, err := m.List(cwd)
	if err != nil {
		return 0, err
	}
	if len(infos) > limit {
		infos = infos[:limit]
	}
	removed := 0
	for _, in := range infos {
		if skipID != "" && in.ID == skipID {
			continue
		}
		if in.Messages > 0 {
			continue
		}
		if err := os.Remove(in.Path); err != nil {
			return removed, err
		}
		// 联动删除 meta 缓存（含可能残留的 tmp），不留孤儿文件；容忍不存在。
		os.Remove(metaPathFor(in.Path))
		os.Remove(metaPathFor(in.Path) + ".tmp")
		removed++
	}
	return removed, nil
}

// List 列出 cwd 下的会话（按最后活跃时间倒序）。
//
// 只读 meta 缓存（<id>.jsonl.meta），不再全量扫描 jsonl——这是会话规模
// 增长时的关键性能点：列表成本从 O(总字节数) 降为 O(会话数 × 单 meta 小读)。
// 无 meta / meta 损坏的会话视为不存在（不兼容旧版数据；被打开后经 open
// 重建 meta 会重新出现）。CreatedAt 承载最后活跃时间（meta.UpdatedAt），
// 与旧实现用 jsonl ModTime 排序的语义一致。
func (m *Manager) List(cwd string) ([]Info, error) {
	dir := m.dirFor(cwd)
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(matches) == 0 {
		return nil, nil
	}

	var infos []Info
	for _, p := range matches {
		meta, err := readMeta(metaPathFor(p))
		if err != nil || meta.ID == "" {
			continue
		}
		updated, _ := time.Parse(time.RFC3339, meta.UpdatedAt)
		infos = append(infos, Info{
			ID: meta.ID, Path: p,
			Model: meta.Model, Provider: meta.Provider, Mode: meta.Mode,
			Title: meta.Title, CreatedAt: updated, Messages: meta.Messages,
		})
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].CreatedAt.After(infos[j].CreatedAt) })
	return infos, nil
}

// open 打开指定会话文件并加载历史。
func (m *Manager) open(path string) (*Session, error) {
	recs, err := readAll(path)
	if err != nil {
		return nil, fmt.Errorf("读取会话失败: %w", err)
	}
	if len(recs) == 0 || recs[0].Type != TypeSession {
		return nil, fmt.Errorf("会话文件格式无效（缺少首行元信息）: %s", path)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开会话失败: %w", err)
	}

	head := recs[0]
	// 只保留模型相关记录（meta 不进入 History，与 Append 行为一致）；
	// meta 中恢复会话标题（title 事件）；旧会话无 title meta 时
	// 从历史首条用户消息补提取（仅内存，不落盘）。
	var hist []Record
	title := ""
	titleSet := false
	msgCount := 0
	for _, r := range recs[1:] {
		switch r.Type {
		case TypeMessage:
			hist = append(hist, r)
			msgCount++
		case TypeToolCall, TypeToolResult:
			hist = append(hist, r)
		case TypeMeta:
			if r.Event == metaTitleEvent {
				title = r.Value
				titleSet = true
			}
		}
	}
	if !titleSet {
		for _, r := range hist {
			if r.Type == TypeMessage && r.Role == "user" {
				title = extractTitle(r.Content)
				titleSet = true
				break
			}
		}
	}
	s := &Session{
		ID: head.ID, Path: path, file: f,
		Mode: head.Mode, Model: head.Model, Provider: head.Provider,
		Title: title, History: hist, titleSet: titleSet,
		CreatedAt: head.CreatedAt, metaPath: metaPathFor(path),
		metaMessages: msgCount,
	}
	// 全量重建 meta 缓存（一致性收敛点：修复崩溃产生的落后/损坏；
	// updated_at 取 jsonl 文件修改时间，与 Continue 的排序口径一致）。
	// 缓存语义：失败静默，不阻塞打开。
	if st, err := os.Stat(path); err == nil {
		s.syncMeta(st.ModTime().Format(time.RFC3339Nano))
	}
	return s, nil
}
