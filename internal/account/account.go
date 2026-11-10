// Package account 管理 Sumus 账号凭证：从 accounts/ 目录加载、落盘、状态跟踪。
package account

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Cred 是单个 Sumus 账号的凭证与运行时状态。
type Cred struct {
	mu sync.Mutex

	// SessionToken 是 better-auth 下发的会话 cookie 值（__Secure-better-auth.session_token）。
	SessionToken string `json:"session_token"`
	// WorkspaceID 是默认工作区 id，来自 /api/app/views/shell。
	WorkspaceID string `json:"workspace_id"`
	// UserID / Email / Name 是账号身份信息，仅用于展示。
	UserID string `json:"user_id,omitempty"`
	Email  string `json:"email,omitempty"`
	Name   string `json:"name,omitempty"`

	// AgentID 是该账号的默认 agent（Sumus 内置 agent），发消息必填。
	AgentID string `json:"agent_id,omitempty"`

	// FilePath 是凭证来源文件，刷新后原子写回此处。
	FilePath string `json:"-"`

	// Balance 是最近一次查到的余额（单位为 1e-6 credit）。
	Balance int64 `json:"balance,omitempty"`
	// CookieExpiresAt 记录 cookie 到期时间（Unix 秒），用于提前告警。
	CookieExpiresAt int64 `json:"cookie_expires_at,omitempty"`

	// 运行时状态（不落盘）
	Disabled   bool   `json:"-"`
	DisableRea string `json:"-"`
	// inFlight 是当前在途请求数。
	inFlight int
	// lastUsed 是最近一次被选中的时间。
	lastUsed time.Time
}

// Lock 供池在改写状态时加锁。
func (c *Cred) Lock() { c.mu.Lock() }

// Unlock 释放 Lock。
func (c *Cred) Unlock() { c.mu.Unlock() }

// Path 返回凭证文件路径（面板删除账号时定位文件用）。
// 注意：Cred 内部已有同名字段 FilePath 且被 json:"-" 隐藏，这里提供只读访问器。
func (c *Cred) Path() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.FilePath
}

// SetDisabled 启用或禁用账号（面板开关用）。
func (c *Cred) SetDisabled(disabled bool, reason string) {
	c.mu.Lock()
	c.Disabled = disabled
	if disabled {
		c.DisableRea = reason
	} else {
		c.DisableRea = ""
	}
	c.mu.Unlock()
}

// Snapshot 返回凭证的只读视图，供面板展示，避免直接暴露内部锁语义。
type Snapshot struct {
	Name            string `json:"name"`
	FilePath        string `json:"file_path"`
	Email           string `json:"email"`
	DisplayName     string `json:"display_name"`
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	Balance         int64  `json:"balance"`
	CookieExpiresAt int64  `json:"cookie_expires_at"`
	Disabled        bool   `json:"disabled"`
	DisabledReason  string `json:"disabled_reason"`
	InFlight        int    `json:"in_flight"`
	Valid           bool   `json:"valid"`
}

// Snapshot 生成当前凭证的只读快照。
// InFlight() 会自行加锁，因此不能在外层持有 c.Lock() 时调用。
func (c *Cred) Snapshot() Snapshot {
	c.Lock()
	s := Snapshot{
		FilePath:        c.FilePath,
		Email:           c.Email,
		DisplayName:     c.Name,
		WorkspaceID:     c.WorkspaceID,
		AgentID:         c.AgentID,
		Balance:         c.Balance,
		CookieExpiresAt: c.CookieExpiresAt,
		Disabled:        c.Disabled,
		DisabledReason:  c.DisableRea,
	}
	c.Unlock()
	s.InFlight = c.InFlight()
	s.Valid = s.FilePath != "" && c.Valid()
	if s.DisplayName == "" {
		s.DisplayName = s.Email
	}
	if s.Name == "" {
		s.Name = filepath.Base(s.FilePath)
	}
	return s
}

// InFlight 返回当前在途请求数。
func (c *Cred) InFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight
}

// Acquire 占用一个在途槽位，返回是否成功。
func (c *Cred) Acquire(max int) bool {
	return c.acquire(max)
}

// acquire 占用一个在途槽位，返回是否成功。
func (c *Cred) acquire(max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if max > 0 && c.inFlight >= max {
		return false
	}
	c.inFlight++
	c.lastUsed = time.Now()
	return true
}

// Release 释放一个在途槽位。
func (c *Cred) Release() { c.release() }

// release 释放一个在途槽位。
func (c *Cred) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight > 0 {
		c.inFlight--
	}
}

// Touch 更新最近使用时间。
func (c *Cred) Touch() {
	c.mu.Lock()
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

// LastUsed 返回最近使用时间。
func (c *Cred) LastUsed() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastUsed
}

// Valid 报告凭证是否可用（有 token 且未禁用）。
func (c *Cred) Valid() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.SessionToken != "" && !c.Disabled
}

// SaveAtomic 把凭证原子写回文件。
func (c *Cred) SaveAtomic() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.FilePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.FilePath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.FilePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.FilePath)
}

// LoadDir 扫描目录下的 *.json 凭证文件。
func LoadDir(dir string) ([]*Cred, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取账号目录 %s: %w", dir, err)
	}

	var creds []*Cred
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取凭证 %s: %w", path, err)
		}
		c := &Cred{FilePath: path}
		if err := json.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("解析凭证 %s: %w", path, err)
		}
		if c.SessionToken == "" {
			continue
		}
		creds = append(creds, c)
	}
	return creds, nil
}

// Store 是线程安全的凭证容器。
type Store struct {
	mu    sync.RWMutex
	dir   string
	creds []*Cred
}

// NewStore 构造空容器，dir 为账号目录（面板新增账号时写入此处）。
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Replace 整体替换凭证集合（启动加载或热加载时用）。
func (s *Store) Replace(creds []*Cred) {
	s.mu.Lock()
	s.creds = creds
	s.mu.Unlock()
}

// List 返回凭证快照。
func (s *Store) List() []*Cred {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Cred, len(s.creds))
	copy(out, s.creds)
	return out
}

// Add 追加一个凭证（去重：同 FilePath 覆盖）。
func (s *Store) Add(c *Cred) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, old := range s.creds {
		if old.FilePath == c.FilePath {
			s.creds[i] = c
			return
		}
	}
	s.creds = append(s.creds, c)
}

// Remove 按文件路径移除凭证，返回被移除的凭证（不存在则返回 nil）。
func (s *Store) Remove(path string) *Cred {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.creds {
		if c.FilePath == path {
			s.creds = append(s.creds[:i], s.creds[i+1:]...)
			return c
		}
	}
	return nil
}

// Find 按文件路径查找凭证（面板启禁用/刷新用）。
func (s *Store) Find(path string) *Cred {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.creds {
		if c.FilePath == path {
			return c
		}
	}
	return nil
}

// Dir 返回账号目录（面板新增账号时写入位置）。
func (s *Store) Dir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dir
}
