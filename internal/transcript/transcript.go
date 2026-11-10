// Package transcript 把每轮对话的请求与回复追加到 JSONL 文件，按会话分文件。
//
// 上游 Sumus 不保留可回读的会话历史（结果靠轮询拿，关掉页面就丢），
// 存档让你事后能查"某次到底问了什么、模型答了什么、用了哪个账号、花了多久"。
package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry 是一轮对话的存档记录。
//
// 采用 JSONL（每行一个 JSON），便于追加写入与流式解析，
// 避免为了追加而去读整个文件再回写。
type Entry struct {
	// Time 是记录写入时间（RFC3339，本地时区）。
	Time string `json:"time"`
	// Conversation 是会话标识（X-Conversation-Id 或内容哈希）。
	Conversation string `json:"conversation"`
	// SessionID 是上游 Sumus 的真实会话 ID。
	SessionID string `json:"session_id,omitempty"`
	// Account 是本次实际使用的账号名。
	Account string `json:"account"`
	// Model 是请求的模型名。
	Model string `json:"model"`
	// Stream 标记客户端是否要求流式返回。
	Stream bool `json:"stream"`
	// DurationMS 是从发起请求到拿到回复的耗时（毫秒）。
	DurationMS int64 `json:"duration_ms"`
	// Request 是请求消息（角色 + 文本）。
	Request []Message `json:"request"`
	// Reply 是模型回复的完整文本。
	Reply string `json:"reply"`
	// MessageID 是上游消息 ID。
	MessageID string `json:"message_id,omitempty"`
	// Error 非空表示本轮失败，此时 Reply 为空。
	Error string `json:"error,omitempty"`
}

// Message 是存档用的消息条目，只保留角色与文本。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Store 管理存档目录与写入。
type Store struct {
	dir      string
	maxBytes int64
	mu       sync.Mutex
}

// New 构造存档器。dir 为空表示不启用存档，此时所有方法都是空操作。
func New(dir string, maxBytes int64) *Store {
	return &Store{dir: dir, maxBytes: maxBytes}
}

// Enabled 报告存档是否启用。
func (s *Store) Enabled() bool { return s != nil && s.dir != "" }

// Dir 返回存档目录。
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Append 把一条记录追加到该会话的 JSONL 文件。
//
// 文件按会话分：<dir>/<sanitized-conversation>.jsonl。
// 体积超上限时滚动到 <name>.1.jsonl，多次滚动递增后缀。
//
// 所有错误都被吞掉并返回——存档失败绝不能影响主请求。
func (s *Store) Append(e Entry) error {
	if !s.Enabled() {
		return nil
	}
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("序列化存档记录: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("创建存档目录: %w", err)
	}

	path := s.pathFor(e.Conversation)
	if s.maxBytes > 0 {
		path = s.rollIfNeeded(path)
	}

	// 追加打开：O_APPEND 保证并发写入不会互相覆盖
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开存档文件: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("写入存档: %w", err)
	}
	return nil
}

// pathFor 把会话标识映射成安全的文件名。
func (s *Store) pathFor(conv string) string {
	name := sanitize(conv)
	if name == "" {
		name = "default"
	}
	return filepath.Join(s.dir, name+".jsonl")
}

// sanitize 过滤文件名中的路径分隔符与特殊字符。
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

// rollIfNeeded 在文件超过体积上限时滚动，返回应写入的路径。
//
// 滚动策略：name.jsonl → name.1.jsonl → name.2.jsonl …
// 只检查当前写入目标，不会回头压缩已写满的历史文件——那些记录本来就该完整保留。
// 若某个历史分片仍在上限内（例如刚滚动完又来了小数据），会继续往里写。
func (s *Store) rollIfNeeded(path string) string {
	info, err := os.Stat(path)
	if err != nil || info.Size() < s.maxBytes {
		return path
	}

	base := strings.TrimSuffix(path, ".jsonl")
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s.%d.jsonl", base, i)
		info, err := os.Stat(candidate)
		if os.IsNotExist(err) {
			// 把当前文件改名为 .1，新的写回原名，保持"最新数据在 name.jsonl"
			if i == 1 {
				_ = os.Rename(path, candidate)
				return path
			}
			// 理论上走不到：只有上一次滚动未完成时才会出现空洞
			return candidate
		}
		if err != nil {
			return path
		}
		if info.Size() < s.maxBytes {
			return candidate
		}
	}
}

// List 返回存档目录下的所有会话文件名（不含路径），按修改时间倒序。
func (s *Store) List() []string {
	if !s.Enabled() {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	type item struct {
		name string
		mod  time.Time
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{name: e.Name(), mod: info.ModTime()})
	}
	// 简单插入排序（文件数通常很少）
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].mod.After(items[j-1].mod); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.name)
	}
	return out
}

// ReadFile 返回指定存档文件的内容（原始 JSONL 文本）。
// 仅允许读取存档目录内的 .jsonl 文件，防止路径穿越。
func (s *Store) ReadFile(name string) ([]byte, error) {
	if !s.Enabled() {
		return nil, os.ErrNotExist
	}
	if strings.ContainsAny(name, "/\\") || !strings.HasSuffix(name, ".jsonl") {
		return nil, fmt.Errorf("非法文件名: %s", name)
	}
	return os.ReadFile(filepath.Join(s.dir, name))
}
