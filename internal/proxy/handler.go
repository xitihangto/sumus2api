// Package proxy 实现 OpenAI 兼容的 HTTP 接口，把请求转译成 Sumus 私有协议。
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"sumus2api/internal/config"
	"sumus2api/internal/upstream"
)

// Handler 是 OpenAI 兼容网关的 HTTP 处理器。
type Handler struct {
	cfg    *config.Config
	client *upstream.Client
	pool   *upstream.Pool
}

// NewHandler 构造处理器。
func NewHandler(cfg *config.Config, client *upstream.Client, pool *upstream.Pool) *Handler {
	return &Handler{cfg: cfg, client: client, pool: pool}
}

// ServeHTTP 路由分发。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// healthz 是探活端点，不鉴权
	if r.URL.Path == "/healthz" {
		h.handleHealth(w, r)
		return
	}

	// 其余路径鉴权
	if h.cfg.APIKey != "" {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == "" || token != h.cfg.APIKey {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "无效的 API key")
			return
		}
	}

	switch r.URL.Path {
	case "/v1/chat/completions":
		h.handleChatCompletions(w, r)
	case "/v1/models":
		h.handleModels(w, r)
	case "/status":
		h.handleStatus(w, r)
	default:
		writeOpenAIError(w, http.StatusNotFound, "not_found", "未知路径: "+r.URL.Path)
	}
}

// chatRequest 是 OpenAI chat/completions 请求体（仅取我们需要的字段）。
type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

// handleChatCompletions 处理对话请求，支持流式与非流式。
func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "读取请求体失败")
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "JSON 解析失败: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "messages 不能为空")
		return
	}

	// 会话 key：客户端传 conversation_id 就用它，否则按内容哈希派生
	convKey := r.Header.Get("X-Conversation-Id")
	if convKey == "" {
		convKey = deriveConvKey(req.Messages)
	}

	// 选号
	cred, err := h.pool.Pick(convKey)
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_account", err.Error())
		return
	}
	if !cred.Acquire(h.cfg.Pool.MaxInFlight) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "account_busy", "账号在途已满")
		return
	}
	defer cred.Release()

	// 组装上游消息
	msgs := make([]upstream.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, upstream.Message{Role: m.Role, Content: m.Content})
	}

	// 已有会话优先
	sessionID := h.pool.SessionFor(convKey)
	if sessionID == "" {
		sessionID = cred.AgentID
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(h.cfg.Upstream.PollTimeoutSeconds)*time.Second)
	defer cancel()

	out, err := h.client.Chat(ctx, cred, cred.WorkspaceID, upstream.ChatInput{
		ConversationKey: convKey,
		SessionID:       sessionID,
		AgentID:         cred.AgentID,
		Messages:        msgs,
		Model:           req.Model,
		Stream:          req.Stream,
	})
	if err != nil {
		if upstream.IsUnauthorized(err) {
			cred.Lock()
			cred.Disabled = true
			cred.DisableRea = "cookie expired"
			cred.Unlock()
			h.pool.Unbind(convKey)
			writeOpenAIError(w, http.StatusUnauthorized, "unauthorized", "Sumus 凭证已过期，需要重新登录")
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	// 记住会话与绑定
	h.pool.RememberSession(convKey, out.SessionID)
	h.pool.Bind(convKey, cred, time.Duration(h.cfg.Session.TTLMinutes)*time.Minute)

	text := out.Message.Text()
	if text == "" {
		text = out.Message.Content
	}

	if req.Stream {
		h.writeStream(w, req.Model, text, out.Message.ID)
		return
	}
	h.writeNonStream(w, req.Model, text, out.Message.ID)
}

// writeStream 以 SSE 形式返回单个 completion chunk + [DONE]。
func (h *Handler) writeStream(w http.ResponseWriter, model, text, msgID string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, _ := w.(http.Flusher)

	chunk := map[string]any{
		"id":      "chatcmpl-" + msgID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{"role": "assistant", "content": text},
				"finish_reason": nil,
			},
		},
	}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher != nil {
		flusher.Flush()
	}

	done := map[string]any{
		"id":      "chatcmpl-" + msgID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
	}
	d, _ := json.Marshal(done)
	fmt.Fprintf(w, "data: %s\n\n", d)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// writeNonStream 返回标准 OpenAI chat.completion 响应。
func (h *Handler) writeNonStream(w http.ResponseWriter, model, text, msgID string) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"id":      "chatcmpl-" + msgID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": text,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// handleModels 返回模型列表。
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET")
		return
	}

	// 优先用第一个可用账号拉取实时模型列表
	var models []upstream.Model
	for _, cred := range h.pool.List() {
		if !cred.Valid() {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		ms, err := h.client.FetchModels(ctx, cred.SessionToken, cred.WorkspaceID)
		cancel()
		if err == nil {
			models = ms
			break
		}
	}

	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": m.Provider,
		})
	}
	if data == nil {
		data = []map[string]any{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}

// handleHealth 返回健康状态。
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	creds := h.pool.List()
	healthy := 0
	for _, c := range creds {
		if c.Valid() {
			healthy++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"healthy": healthy,
		"total":   len(creds),
		"service": "sumus2api",
	})
}

// handleStatus 返回账号池详情（供运维排查）。
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	creds := h.pool.List()
	rows := make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		// 注意：InFlight() 自行加锁，不能在外层持有 c.Lock() 调用（会死锁）
		c.Lock()
		row := map[string]any{
			"email":        c.Email,
			"name":         c.Name,
			"workspace_id": c.WorkspaceID,
			"agent_id":     c.AgentID,
			"balance":      c.Balance,
			"disabled":     c.Disabled,
		}
		if c.Disabled {
			row["disabled_reason"] = c.DisableRea
		}
		c.Unlock()
		row["in_flight"] = c.InFlight()
		rows = append(rows, row)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"accounts": rows})
}

// writeOpenAIError 输出 OpenAI 格式的错误响应。
func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

// deriveConvKey 从消息内容派生会话 key，保证同一对话稳定命中同一账号。
func deriveConvKey(msgs []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) string {
	if len(msgs) == 0 {
		return ""
	}
	first := ""
	for _, m := range msgs {
		if m.Role == "user" {
			first = m.Content
			break
		}
	}
	if first == "" {
		first = msgs[0].Content
	}
	// 简单哈希：取前 64 字符
	if len(first) > 64 {
		first = first[:64]
	}
	return "d-" + first
}
