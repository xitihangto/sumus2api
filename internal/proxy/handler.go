// Package proxy 实现 OpenAI 兼容的 HTTP 接口，把请求转译成 Sumus 私有协议。
package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sumus2api/internal/account"
	"sumus2api/internal/config"
	"sumus2api/internal/transcript"
	"sumus2api/internal/upstream"
)

// Handler 是 OpenAI 兼容网关的 HTTP 处理器。
type Handler struct {
	cfg        *config.Config
	client     *upstream.Client
	pool       *upstream.Pool
	store      *account.Store
	transcript *transcript.Store
}

// NewHandler 构造处理器。store 用于面板新增账号时定位写入目录。
func NewHandler(cfg *config.Config, client *upstream.Client, pool *upstream.Pool, store *account.Store) *Handler {
	return &Handler{
		cfg:        cfg,
		client:     client,
		pool:       pool,
		store:      store,
		transcript: transcript.New(cfg.Transcript.Dir, cfg.Transcript.MaxBytesPerFile),
	}
}

// ServeHTTP 路由分发。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 免鉴权白名单：探活与面板静态页。
	// 面板页只是 HTML 壳，真正的数据写操作由 /admin/api/* 单独鉴权。
	if r.URL.Path == "/healthz" {
		h.handleHealth(w, r)
		return
	}
	if r.URL.Path == "/admin" || r.URL.Path == "/admin/" {
		h.servePanel(w, r)
		return
	}

	// 其余路径鉴权。管理 API 额外支持 ?key=，
	// 因为浏览器无法自定义 Authorization 头（面板需据此在设了 key 时也能用）。
	if h.cfg.APIKey != "" {
		if !h.authorized(r) {
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
		// /admin/api/* 的动态子路径
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			h.handleAdmin(w, r)
			return
		}
		writeOpenAIError(w, http.StatusNotFound, "not_found", "未知路径: "+r.URL.Path)
	}
}

// authorized 校验请求是否携带正确的凭据。
// 优先看 Authorization 头，其次看 ?key=（仅对 /admin/api/* 开放）。
func (h *Handler) authorized(r *http.Request) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token != "" && token == h.cfg.APIKey {
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/admin/api/") {
		if k := r.URL.Query().Get("key"); k != "" && k == h.cfg.APIKey {
			return true
		}
	}
	return false
}

// chatRequest 是 OpenAI chat/completions 请求体（仅取我们需要的字段）。
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// chatMessage 支持两种 content 形式：
//
//	"content": "纯文本"                                  （OpenAI 文本）
//	"content": [{"type":"text",...},{"type":"image_url",...}]  （OpenAI 多模态）
//
// 用 json.RawMessage 接住再分流解析，避免直接声明成 any 后丢掉类型安全。
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentParts 从 content 字段解析出纯文本与文件列表。
//
// 字符串形式直接返回；数组形式抽取所有 text 片段，并把 image_url / input_file
// 收集为待上传文件。无法识别的片段被忽略而不是报错——保证老客户端不受影响。
func (m chatMessage) contentParts() (text string, files []contentFile) {
	if len(m.Content) == 0 {
		return "", nil
	}
	// 字符串形式
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, nil
	}
	// 数组形式
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
		InputFile *struct {
			Filename string `json:"filename"`
			Data     string `json:"data"`    // base64（可带 data: 前缀）
			FileID   string `json:"file_id"` // 已上传文件的 ID（/files 端点）
		} `json:"input_file"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		// 既不是字符串也不是数组：当作空内容，避免整轮失败
		return "", nil
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			sb.WriteString(p.Text)
		case "image_url":
			if p.ImageURL != nil && p.ImageURL.URL != "" {
				// image_url 既可能是 http(s) URL，也可能是 data:image/...;base64, 前缀
				if strings.HasPrefix(p.ImageURL.URL, "data:") {
					files = append(files, contentFile{DataB64: p.ImageURL.URL, Filename: guessFilenameFromDataURI(p.ImageURL.URL)})
				} else {
					files = append(files, contentFile{URL: p.ImageURL.URL})
				}
			}
		case "input_file":
			if p.InputFile != nil {
				files = append(files, contentFile{
					Filename: p.InputFile.Filename,
					DataB64:  p.InputFile.Data,
					FileID:   p.InputFile.FileID,
				})
			}
		}
	}
	return sb.String(), files
}

// contentFile 是消息里引用的一个文件，三种来源之一：
// URL（http/https 图片）、DataB64（内联 base64）、FileID（已上传文件 ID）。
type contentFile struct {
	URL      string
	Filename string
	DataB64  string
	FileID   string
}

// handleChatCompletions 处理对话请求，支持流式与非流式。
func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST")
		return
	}

	start := time.Now()

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

	// 组装上游消息，同时把消息里引用的文件收出来
	msgs := make([]upstream.Message, 0, len(req.Messages))
	var pendingFiles []contentFile
	for _, m := range req.Messages {
		text, files := m.contentParts()
		msgs = append(msgs, upstream.Message{Role: m.Role, Content: text})
		pendingFiles = append(pendingFiles, files...)
	}

	// 已有会话优先
	sessionID := h.pool.SessionFor(convKey)
	if sessionID == "" {
		sessionID = cred.AgentID
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(h.cfg.Upstream.PollTimeoutSeconds)*time.Second)
	defer cancel()

	// 上传附件（如果有）。上传走同一个 ctx，失败则整轮失败——
	// 半截附件比没有附件更糟。
	attachmentIDs, err := h.uploadAttachments(ctx, cred, pendingFiles)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upload_failed", "附件上传失败: "+err.Error())
		return
	}

	out, err := h.client.Chat(ctx, cred, cred.WorkspaceID, upstream.ChatInput{
		ConversationKey: convKey,
		SessionID:       sessionID,
		AgentID:         cred.AgentID,
		Messages:        msgs,
		Model:           req.Model,
		Stream:          req.Stream,
		AttachmentIDs:   attachmentIDs,
	})
	if err != nil {
		if upstream.IsUnauthorized(err) {
			cred.Lock()
			cred.Disabled = true
			cred.DisableRea = "cookie expired"
			cred.Unlock()
			h.pool.Unbind(convKey)
			h.saveTranscript(convKey, cred, req, "", nil, time.Since(start), "unauthorized: cookie expired")
			writeOpenAIError(w, http.StatusUnauthorized, "unauthorized", "Sumus 凭证已过期，需要重新登录")
			return
		}
		h.saveTranscript(convKey, cred, req, "", nil, time.Since(start), err.Error())
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

	// 异步存档：不阻塞响应，失败也不影响主流程
	h.saveTranscript(convKey, cred, req, text, out, time.Since(start), "")

	if req.Stream {
		h.writeStream(w, req.Model, text, out.Message.ID)
		return
	}
	h.writeNonStream(w, req.Model, text, out.Message.ID)
}

// uploadAttachments 把消息里引用的文件上传到 Sumus，返回文件 ID 列表。
//
// 支持三种来源：
//   - FileID：客户端已通过 OpenAI /files 上传，直接引用（网关未实现 /files，
//     传入时会报错提示，避免静默丢弃）
//   - DataB64：内联 base64，解码后走完整上传流程
//   - URL：http(s) 图片，先下载再上传
//
// 没有文件时直接返回 nil，不做任何上游调用。
func (h *Handler) uploadAttachments(ctx context.Context, cred *account.Cred, files []contentFile) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}

	workspaceID := cred.WorkspaceID
	targetID := cred.AgentID

	ids := make([]string, 0, len(files))
	for i, f := range files {
		name := f.Filename
		if name == "" {
			name = fmt.Sprintf("attachment-%d", i+1)
		}

		var data []byte
		switch {
		case f.FileID != "":
			// OpenAI /files 上传的文件。Sumus 没有对应通道，直接透传会失效，
			// 明确报错好过让模型看到一条"附件无法显示"。
			return nil, fmt.Errorf("不支持 file_id 引用（%s）：请先下载后以 base64 内联传入", name)

		case f.DataB64 != "":
			raw, err := decodeBase64(f.DataB64)
			if err != nil {
				return nil, fmt.Errorf("解码 %s 的 base64: %w", name, err)
			}
			data = raw

		case f.URL != "":
			raw, err := h.downloadURL(ctx, f.URL)
			if err != nil {
				return nil, fmt.Errorf("下载 %s: %w", name, err)
			}
			data = raw
			// URL 通常带扩展名，补全文件名
			if !strings.Contains(name, ".") {
				name = name + guessExtFromURL(f.URL)
			}

		default:
			continue
		}

		res, err := h.client.UploadFile(ctx, cred.SessionToken, workspaceID, targetID, name, data, "")
		if err != nil {
			return nil, fmt.Errorf("上传 %s: %w", name, err)
		}
		ids = append(ids, res.ID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return ids, nil
}

// decodeBase64 解析可能带 data URI 前缀的 base64 字符串。
func decodeBase64(s string) ([]byte, error) {
	if i := strings.Index(s, ","); i >= 0 && strings.HasPrefix(s, "data:") {
		s = s[i+1:]
	}
	return base64.StdEncoding.DecodeString(s)
}

// guessFilenameFromDataURI 从 data URI 的 MIME 推断一个文件名。
func guessFilenameFromDataURI(uri string) string {
	// 形如 data:image/png;base64,xxxx
	if !strings.HasPrefix(uri, "data:") {
		return ""
	}
	meta := uri[5:]
	if i := strings.Index(meta, ";"); i >= 0 {
		meta = meta[:i]
	}
	ext := ".bin"
	switch meta {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	case "application/pdf":
		ext = ".pdf"
	case "text/plain":
		ext = ".txt"
	}
	return "image" + ext
}

// downloadURL 下载图片/文件二进制内容，带大小上限防止内存爆炸。
// 只接受 http/https，拒绝 file:// 等本地协议（防 SSRF 读取本机文件）。
func (h *Handler) downloadURL(ctx context.Context, rawURL string) ([]byte, error) {
	const maxBytes = 20 << 20 // 20 MiB
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("不支持的协议: %s（仅 http/https）", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	// 很多图床/CDN 会拒绝无 User-Agent 的请求（403），补一个常见 UA
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
}

// guessExtFromURL 从 URL 路径猜扩展名。
func guessExtFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	p := u.Path
	if i := strings.LastIndex(p, "."); i >= 0 && i < len(p)-1 && len(p)-i <= 6 {
		return p[i:]
	}
	return ""
}

// saveTranscript 把一轮对话写入存档。异步执行。
func (h *Handler) saveTranscript(convKey string, cred *account.Cred, req chatRequest, reply string, out *upstream.ChatOutput, dur time.Duration, errMsg string) {
	if h.transcript == nil || !h.transcript.Enabled() {
		return
	}
	// 用快照读账号名，避免与 warmup 的写入竞争
	acctName := ""
	if snap := cred.Snapshot(); snap.Name != "" {
		acctName = snap.Name
	} else if snap.Email != "" {
		acctName = snap.Email
	}

	entry := transcript.Entry{
		Conversation: convKey,
		Account:      acctName,
		Model:        req.Model,
		Stream:       req.Stream,
		DurationMS:   dur.Milliseconds(),
		Reply:        reply,
		Error:        errMsg,
	}
	if out != nil {
		entry.SessionID = out.SessionID
		entry.MessageID = out.Message.ID
	}
	for _, m := range req.Messages {
		text, files := m.contentParts()
		// 存档只记文件名，不记二进制内容——否则存档会膨胀到不可用
		var names []string
		for _, f := range files {
			n := f.Filename
			if n == "" {
				n = "unnamed"
			}
			names = append(names, n)
		}
		entry.Request = append(entry.Request, transcript.Message{
			Role:    m.Role,
			Content: text,
			Files:   names,
		})
	}
	if entry.Request == nil {
		entry.Request = []transcript.Message{}
	}
	go func() {
		if err := h.transcript.Append(entry); err != nil {
			log.Printf("[transcript] 存档失败: %v", err)
		}
	}()
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

// warmupCred 拉取账号身份、工作区、agent 与余额，补齐凭证字段并落盘。
// 启动预热与面板新增/刷新共用。
func (h *Handler) warmupCred(ctx context.Context, c *account.Cred) error {
	return WarmupCred(ctx, h.client, c)
}

// WarmupCred 拉取账号身份、工作区、agent 与余额，补齐凭证字段并落盘。
// 导出以便 main 启动预热复用，避免重复实现。
func WarmupCred(ctx context.Context, client *upstream.Client, c *account.Cred) error {
	me, err := client.FetchMe(ctx, c.SessionToken)
	if err != nil {
		return fmt.Errorf("拉取身份: %w", err)
	}
	c.Lock()
	c.UserID = me.ID
	c.Email = me.Email
	c.Name = me.Name
	c.Unlock()

	if bs, err := client.FetchBalance(ctx, c.SessionToken, ""); err == nil {
		c.Lock()
		c.Balance = parseInt64(bs.Balance)
		c.Unlock()
	}

	wss, err := client.FetchWorkspaces(ctx, c.SessionToken)
	if err == nil && len(wss) > 0 {
		c.Lock()
		c.WorkspaceID = wss[0].ID
		c.Unlock()
	}

	agents, err := client.FetchAgents(ctx, c.SessionToken, c.WorkspaceID)
	if err == nil && len(agents) > 0 {
		c.Lock()
		c.AgentID = agents[0].ID
		c.Unlock()
	}

	_ = c.SaveAtomic()
	return nil
}

// parseInt64 宽松解析整数字符串（余额字段是字符串形式）。
func parseInt64(s string) int64 {
	var n int64
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
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
func deriveConvKey(msgs []chatMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	first := ""
	for _, m := range msgs {
		if m.Role == "user" {
			first, _ = m.contentParts()
			break
		}
	}
	if first == "" {
		first, _ = msgs[0].contentParts()
	}
	// 简单哈希：取前 64 字符
	if len(first) > 64 {
		first = first[:64]
	}
	return "d-" + first
}
