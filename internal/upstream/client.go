// Package upstream 封装 Sumus 私有协议：发消息 + 轮询取结果。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 是与 Sumus 上游通信的 HTTP 客户端。
type Client struct {
	baseURL            string
	version            string
	stage              string
	locale             string
	timezone           string
	pollIntervalMs     int
	pollTimeoutSeconds int
	http               *http.Client
	userAgent          string
}

// New 构造上游客户端。
func New(baseURL, version, stage, locale, timezone string, timeout time.Duration, pollIntervalMs, pollTimeoutSeconds int) *Client {
	return &Client{
		baseURL:            strings.TrimRight(baseURL, "/"),
		version:            version,
		stage:              stage,
		locale:             locale,
		timezone:           timezone,
		pollIntervalMs:     pollIntervalMs,
		pollTimeoutSeconds: pollTimeoutSeconds,
		http: &http.Client{
			Timeout: timeout,
		},
		userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
	}
}

// commonHeaders 构造 Sumus 要求的 X-Client-* 头族。
// workspaceID 为空时不带 X-Workspace-Id。
func (c *Client) commonHeaders(workspaceID string) http.Header {
	h := http.Header{}
	h.Set("X-Client-Platform", "web")
	if c.version != "" {
		h.Set("X-Client-Version", c.version)
	}
	if c.stage != "" {
		h.Set("X-Client-Stage", c.stage)
	}
	if c.locale != "" {
		h.Set("X-Client-Locale", c.locale)
	}
	if c.timezone != "" {
		h.Set("X-Client-Timezone", c.timezone)
	}
	if workspaceID != "" {
		h.Set("X-Workspace-Id", workspaceID)
	}
	h.Set("User-Agent", c.userAgent)
	h.Set("Accept", "application/json")
	return h
}

// APIError 是上游返回的结构化错误。
type APIError struct {
	Status  int    `json:"-"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Error 实现 error 接口。
func (e *APIError) Error() string {
	return fmt.Sprintf("sumus api %d: %s: %s", e.Status, e.Type, e.Message)
}

// Me 是 /api/app/me 的响应。
type Me struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

// FetchMe 拉取当前账号身份。
func (c *Client) FetchMe(ctx context.Context, sessionToken string) (*Me, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/app/me", nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.commonHeaders("")
	req.Header.Set("Cookie", "__Secure-better-auth.session_token="+sessionToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, body)
	}
	var me Me
	if err := json.Unmarshal(body, &me); err != nil {
		return nil, fmt.Errorf("解析 me: %w", err)
	}
	return &me, nil
}

// BillingStatus 是 /api/app/billing/status 的响应。
type BillingStatus struct {
	Enabled  bool   `json:"enabled"`
	Depleted bool   `json:"depleted"`
	Balance  string `json:"balance"`
	Plan     string `json:"plan"`
}

// FetchBalance 查询账号余额。
func (c *Client) FetchBalance(ctx context.Context, sessionToken, workspaceID string) (*BillingStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/app/billing/status", nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.commonHeaders(workspaceID)
	req.Header.Set("Cookie", "__Secure-better-auth.session_token="+sessionToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, body)
	}
	var bs BillingStatus
	if err := json.Unmarshal(body, &bs); err != nil {
		return nil, fmt.Errorf("解析 billing: %w", err)
	}
	return &bs, nil
}

// Model 描述一个可用模型。
type Model struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Provider        string  `json:"provider"`
	Reasoning       bool    `json:"reasoning"`
	RequiresUpgrade bool    `json:"requires_upgrade"`
	PromptPrice     float64 `json:"prompt,omitempty"`
	CompletionPrice float64 `json:"completion,omitempty"`
}

// FetchModels 拉取模型列表。
func (c *Client) FetchModels(ctx context.Context, sessionToken, workspaceID string) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/app/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.commonHeaders(workspaceID)
	req.Header.Set("Cookie", "__Secure-better-auth.session_token="+sessionToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, body)
	}
	var wrap struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("解析 models: %w", err)
	}
	return wrap.Models, nil
}

// Workspace 是工作区信息。
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// FetchWorkspaces 拉取账号的工作区列表。
func (c *Client) FetchWorkspaces(ctx context.Context, sessionToken string) ([]Workspace, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/app/workspaces", nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.commonHeaders("")
	req.Header.Set("Cookie", "__Secure-better-auth.session_token="+sessionToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, body)
	}
	var wrap struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("解析 workspaces: %w", err)
	}
	return wrap.Workspaces, nil
}

// Agent 是 AI 队友信息。
type Agent struct {
	ID       string `json:"id"`
	Callsign string `json:"callsign"`
	Name     string `json:"name"`
}

// FetchAgents 拉取工作区下的 agent 列表。
func (c *Client) FetchAgents(ctx context.Context, sessionToken, workspaceID string) ([]Agent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/app/agents", nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.commonHeaders(workspaceID)
	req.Header.Set("Cookie", "__Secure-better-auth.session_token="+sessionToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, body)
	}
	var wrap struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("解析 agents: %w", err)
	}
	return wrap.Agents, nil
}

// Session 是 /api/app/sessions 的列表项。
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Preview   string `json:"preview"`
	Channel   string `json:"channel"`
	Running   bool   `json:"running"`
	LastSeq   int64  `json:"last_entry_seq"`
	CreatedAt int64  `json:"created_at"`
}

// FetchSessions 列出某 agent 的会话。
func (c *Client) FetchSessions(ctx context.Context, sessionToken, workspaceID, agentID string) ([]Session, error) {
	q := url.Values{}
	q.Set("agent_id", agentID)
	endpoint := c.baseURL + "/api/app/sessions?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.commonHeaders(workspaceID)
	req.Header.Set("Cookie", "__Secure-better-auth.session_token="+sessionToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, body)
	}
	var wrap struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("解析 sessions: %w", err)
	}
	return wrap.Sessions, nil
}

// SendMessageInput 是发消息的入参。
type SendMessageInput struct {
	// SessionID 可以是真实 session_id，也可以是 agent_id（首条消息会自动建会话）。
	SessionID string
	AgentID   string
	Content   string
	Model     string
}

// SendMessageResult 是发消息的响应。
type SendMessageResult struct {
	Status       string `json:"status"`
	FirstMessage bool   `json:"first_message"`
}

// SendMessage 提交一条消息。上游异步执行，立即返回 202。
func (c *Client) SendMessage(ctx context.Context, sessionToken, workspaceID string, in SendMessageInput) (*SendMessageResult, error) {
	payload := map[string]any{
		"agent_id": in.AgentID,
		"content":  in.Content,
	}
	if in.Model != "" {
		payload["model"] = in.Model
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	endpoint := c.baseURL + "/api/app/sessions/" + url.PathEscape(in.SessionID) + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.commonHeaders(workspaceID)
	req.Header.Set("Cookie", "__Secure-better-auth.session_token="+sessionToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.baseURL)
	req.Header.Set("Referer", c.baseURL+"/app")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, respBody)
	}
	var out SendMessageResult
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("解析 send: %w", err)
	}
	return &out, nil
}

// Block 是消息内容块。
type Block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Message 是一条会话消息。
// Role 字段用于承接 OpenAI 入参（author 与 role 二选一，Text 为纯文本快捷字段）。
type Message struct {
	ID         string  `json:"id"`
	CreatedAt  int64   `json:"createdAt"`
	Author     string  `json:"author"`
	AuthorName string  `json:"authorName"`
	Blocks     []Block `json:"blocks"`

	// 以下为 OpenAI 入参映射字段，不参与上游 JSON 序列化
	Role        string `json:"role,omitempty"`
	Content     string `json:"content,omitempty"`
	ContentText string `json:"-"`
}

// Text 把消息的所有文本块拼接起来。
func (m Message) Text() string {
	// 上游消息（有 Blocks）优先从 Blocks 拼接
	if len(m.Blocks) > 0 {
		var sb strings.Builder
		for _, b := range m.Blocks {
			if b.Type == "text" && b.Text != "" {
				sb.WriteString(b.Text)
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	// OpenAI 入参映射字段
	if m.Content != "" {
		return m.Content
	}
	return m.ContentText
}

// FetchMessagesResult 是消息拉取响应。
type FetchMessagesResult struct {
	Messages  []Message `json:"messages"`
	HasOlder  bool      `json:"has_older"`
	HasNewer  bool      `json:"has_newer"`
	OldestSeq int64     `json:"oldest_seq"`
	NewestSeq int64     `json:"newest_seq"`
}

// FetchMessages 拉取 after_seq 之后的新消息。
func (c *Client) FetchMessages(ctx context.Context, sessionToken, workspaceID, sessionID string, afterSeq int64) (*FetchMessagesResult, error) {
	q := url.Values{}
	q.Set("after_seq", fmt.Sprintf("%d", afterSeq))
	endpoint := c.baseURL + "/api/app/sessions/" + url.PathEscape(sessionID) + "/messages?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.commonHeaders(workspaceID)
	req.Header.Set("Cookie", "__Secure-better-auth.session_token="+sessionToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, body)
	}
	var out FetchMessagesResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析 messages: %w", err)
	}
	return &out, nil
}

// parseAPIError 把上游错误体解析成 APIError。
func parseAPIError(status int, body []byte) error {
	var wrap struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrap); err == nil && wrap.Error.Message != "" {
		return &APIError{Status: status, Type: wrap.Error.Type, Message: wrap.Error.Message}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return &APIError{Status: status, Type: "unknown", Message: msg}
}

// IsUnauthorized 判断是否是鉴权失败（cookie 过期）。
func IsUnauthorized(err error) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Status == http.StatusUnauthorized
}
