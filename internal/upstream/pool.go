package upstream

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"sumus2api/internal/account"
)

// ErrNoAccount 表示池里没有可用账号。
var ErrNoAccount = errors.New("no available sumus account")

// Pool 是账号池：负责选号、在途限流、会话粘性。
type Pool struct {
	store       *account.Store
	maxInFlight int
	strategy    string

	mu      sync.Mutex
	rrIndex int
	// sticky 把 conversation key 映射到账号指纹（email 或 token 前缀）。
	sticky map[string]stickyEntry
	// sessionByConv 记录会话 key 对应的 Sumus session_id，避免每次都新建。
	sessionByConv map[string]string
}

type stickyEntry struct {
	credKey   string
	expiresAt time.Time
}

// credKey 返回账号的稳定标识（优先 email，其次 token 前 8 位）。
func credKey(c *account.Cred) string {
	if c.Email != "" {
		return c.Email
	}
	if len(c.SessionToken) >= 8 {
		return c.SessionToken[:8]
	}
	return c.UserID
}

// NewPool 构造账号池。
func NewPool(store *account.Store, maxInFlight int, strategy string) *Pool {
	return &Pool{
		store:         store,
		maxInFlight:   maxInFlight,
		strategy:      strategy,
		sticky:        make(map[string]stickyEntry),
		sessionByConv: make(map[string]string),
	}
}

// List 返回池内所有账号。
func (p *Pool) List() []*account.Cred {
	return p.store.List()
}

// Add 把新凭证加入池（面板新增账号时用）。
func (p *Pool) Add(c *account.Cred) { p.store.Add(c) }

// Find 按文件路径查找凭证（面板启禁用/刷新时用）。
func (p *Pool) Find(path string) *account.Cred { return p.store.Find(path) }

// Remove 按文件路径移除凭证（面板删除账号时用）。
func (p *Pool) Remove(path string) *account.Cred { return p.store.Remove(path) }

// Store 暴露底层容器（供面板新增账号时定位目录）。
func (p *Pool) Store() *account.Store { return p.store }

// Pick 选一个可用账号。conversationKey 非空时优先复用粘性绑定。
func (p *Pool) Pick(conversationKey string) (*account.Cred, error) {
	creds := p.store.List()
	if len(creds) == 0 {
		return nil, ErrNoAccount
	}

	// 粘性命中：conversationKey 已绑定且未过期，且该账号仍可用
	if conversationKey != "" {
		p.mu.Lock()
		entry, ok := p.sticky[conversationKey]
		if ok && time.Now().Before(entry.expiresAt) {
			p.mu.Unlock()
			for _, c := range creds {
				if credKey(c) == entry.credKey && c.Valid() && c.InFlight() < p.maxInFlight {
					return c, nil
				}
			}
		} else if ok {
			delete(p.sticky, conversationKey)
			p.mu.Unlock()
		} else {
			p.mu.Unlock()
		}
	}

	// 无粘性或绑定失效：按策略选号
	candidates := make([]*account.Cred, 0, len(creds))
	for _, c := range creds {
		if c.Valid() && c.InFlight() < p.maxInFlight {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNoAccount
	}

	var picked *account.Cred
	switch p.strategy {
	case "round_robin":
		p.mu.Lock()
		for i := 0; i < len(candidates); i++ {
			idx := (p.rrIndex + i) % len(candidates)
			p.rrIndex = (p.rrIndex + 1) % len(candidates)
			picked = candidates[idx]
			break
		}
		p.mu.Unlock()
	case "sequential_exhaust":
		// 选最久没用的（简单近似顺序耗尽）
		picked = candidates[0]
		for _, c := range candidates[1:] {
			if c.LastUsed().Before(picked.LastUsed()) {
				picked = c
			}
		}
	default: // weighted_random
		// 权重：闲置越久权重越高，上限 5
		weights := make([]float64, len(candidates))
		var total float64
		now := time.Now()
		for i, c := range candidates {
			idle := now.Sub(c.LastUsed()).Hours()
			w := 1.0 + idle*0.5
			if w > 5 {
				w = 5
			}
			weights[i] = w
			total += w
		}
		r := rand.Float64() * total
		var acc float64
		for i, w := range weights {
			acc += w
			if r <= acc {
				picked = candidates[i]
				break
			}
		}
		if picked == nil {
			picked = candidates[len(candidates)-1]
		}
	}

	if picked == nil {
		return nil, ErrNoAccount
	}
	return picked, nil
}

// Bind 记录 conversationKey 到 picked 的粘性绑定。
func (p *Pool) Bind(conversationKey string, picked *account.Cred, ttl time.Duration) {
	if conversationKey == "" {
		return
	}
	p.mu.Lock()
	p.sticky[conversationKey] = stickyEntry{
		credKey:   credKey(picked),
		expiresAt: time.Now().Add(ttl),
	}
	p.mu.Unlock()
}

// Unbind 解除粘性绑定（账号出错时调用）。
func (p *Pool) Unbind(conversationKey string) {
	if conversationKey == "" {
		return
	}
	p.mu.Lock()
	delete(p.sticky, conversationKey)
	p.mu.Unlock()
}

// RememberSession 记录 conversationKey 对应的 Sumus session_id。
func (p *Pool) RememberSession(conversationKey, sessionID string) {
	if conversationKey == "" || sessionID == "" {
		return
	}
	p.mu.Lock()
	p.sessionByConv[conversationKey] = sessionID
	p.mu.Unlock()
}

// SessionFor 取出 conversationKey 对应的 session_id。
func (p *Pool) SessionFor(conversationKey string) string {
	if conversationKey == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionByConv[conversationKey]
}

// GC 清理过期绑定。
func (p *Pool) GC() {
	now := time.Now()
	p.mu.Lock()
	for k, v := range p.sticky {
		if now.After(v.expiresAt) {
			delete(p.sticky, k)
		}
	}
	p.mu.Unlock()
}

// StartGC 启动后台 GC。
func (p *Pool) StartGC(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.GC()
			}
		}
	}()
}

// ChatInput 是一次对话请求。
type ChatInput struct {
	ConversationKey string
	SessionID       string
	AgentID         string
	Messages        []Message
	Model           string
	Stream          bool
}

// ChatOutput 是对话结果。
type ChatOutput struct {
	Message   Message
	SessionID string
}

// Chat 执行一次完整的「发消息 → 轮询取结果」。
// 调用方负责 acquire/release 在途槽位。
func (c *Client) Chat(ctx context.Context, cred *account.Cred, workspaceID string, in ChatInput) (*ChatOutput, error) {
	// 首次用 agent_id 当 session_id，上游会自动建会话
	sessionID := in.SessionID
	if sessionID == "" {
		sessionID = in.AgentID
	}
	if sessionID == "" {
		return nil, errors.New("session_id 与 agent_id 均为空")
	}

	// 取用户最后一条消息作为 content
	content := lastUserText(in.Messages)
	if content == "" {
		return nil, errors.New("没有可发送的用户消息")
	}

	// 发消息前先记录当前最新 seq，轮询时只认这个 seq 之后的新消息，
	// 否则会拿到上一轮的旧回复（历史消息全部在 after_seq=0 里）。
	var baselineSeq int64
	if pre, err := c.FetchMessages(ctx, cred.SessionToken, workspaceID, sessionID, 0); err == nil {
		baselineSeq = pre.NewestSeq
	}

	if _, err := c.SendMessage(ctx, cred.SessionToken, workspaceID, SendMessageInput{
		SessionID: sessionID,
		AgentID:   in.AgentID,
		Content:   content,
		Model:     in.Model,
	}); err != nil {
		return nil, err
	}

	// 轮询直到出现 agent 回复
	pollInterval := time.Duration(c.pollIntervalMs) * time.Millisecond
	deadline := time.Now().Add(time.Duration(c.pollTimeoutSeconds) * time.Second)

	lastSeq := baselineSeq
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待 agent 回复超时（%s）", time.Duration(c.pollTimeoutSeconds)*time.Second)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}

		out, err := c.FetchMessages(ctx, cred.SessionToken, workspaceID, sessionID, lastSeq)
		if err != nil {
			return nil, err
		}
		// 找最后一条 agent 消息
		var lastAgent *Message
		for i := len(out.Messages) - 1; i >= 0; i-- {
			if out.Messages[i].Author == "agent" {
				m := out.Messages[i]
				lastAgent = &m
				break
			}
		}
		if lastAgent != nil {
			return &ChatOutput{
				Message:   *lastAgent,
				SessionID: sessionID,
			}, nil
		}
		if out.NewestSeq > lastSeq {
			lastSeq = out.NewestSeq
		}
		// 会话已在执行中：继续等；否则超时退出
	}
}

// lastUserText 提取最后一条用户消息的文本。
func lastUserText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Author == "user" || msgs[i].Role == "user" {
			if msgs[i].Content != "" {
				return msgs[i].Content
			}
			if msgs[i].ContentText != "" {
				return msgs[i].ContentText
			}
		}
	}
	return ""
}

// counter 用于生成递增的 id 后缀。
var counter atomic.Int64
