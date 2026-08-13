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
	CreatedAt time.Time
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

// List 列出 cwd 下的会话（按修改时间倒序）。
func (m *Manager) List(cwd string) ([]Info, error) {
	dir := m.dirFor(cwd)
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(matches) == 0 {
		return nil, nil
	}

	var infos []Info
	for _, p := range matches {
		recs, err := readAll(p)
		if err != nil || len(recs) == 0 {
			continue
		}
		head := recs[0]
		if head.Type != TypeSession {
			continue
		}
		st, _ := os.Stat(p)
		ts, _ := time.Parse(time.RFC3339, head.CreatedAt)
		messages := 0
		for _, r := range recs[1:] {
			if r.Type == TypeMessage {
				messages++
			}
		}
		infos = append(infos, Info{
			ID: head.ID, Path: p,
			Model: head.Model, Provider: head.Provider, Mode: head.Mode,
			CreatedAt: ts, Messages: messages,
		})
		if st != nil {
			infos[len(infos)-1].CreatedAt = st.ModTime()
		}
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
	// 只保留模型相关记录（meta 不进入 History，与 Append 行为一致）
	var hist []Record
	for _, r := range recs[1:] {
		switch r.Type {
		case TypeMessage, TypeToolCall, TypeToolResult:
			hist = append(hist, r)
		}
	}
	s := &Session{
		ID: head.ID, Path: path, file: f,
		Mode: head.Mode, Model: head.Model, Provider: head.Provider,
		History: hist,
	}
	return s, nil
}
