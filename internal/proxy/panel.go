package proxy

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sumus2api/internal/account"
)

// panelFS 内嵌账号池管理面板的单页资源。
//
//go:embed panel/*
var panelFS embed.FS

// handleAdmin 提供账号池管理接口与面板页面。
//
// 路由：
//
//	GET  /admin/                          → 面板页面（HTML，免鉴权）
//	GET  /admin/api/accounts              → 账号列表 + 接入配置（支持 ?key= 供浏览器用）
//	POST /admin/api/accounts              → 新增账号
//	POST /admin/api/accounts/{name}/enable   → 启用
//	POST /admin/api/accounts/{name}/disable  → 禁用
//	POST /admin/api/accounts/{name}/refresh  → 刷新余额
//	DEL  /admin/api/accounts/{name}          → 删除
func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin" || r.URL.Path == "/admin/" {
		h.servePanel(w, r)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/admin/api/") {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "未知路径: "+r.URL.Path)
		return
	}

	if r.URL.Path == "/admin/api/accounts" {
		switch r.Method {
		case http.MethodGet:
			h.adminList(w, r)
		case http.MethodPost:
			h.adminAdd(w, r)
		default:
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET/POST")
		}
		return
	}

	h.adminAccountAction(w, r)
}

// servePanel 返回内嵌的面板 HTML。
func (h *Handler) servePanel(w http.ResponseWriter, r *http.Request) {
	data, err := panelFS.ReadFile("panel/index.html")
	if err != nil {
		http.Error(w, "面板资源缺失: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// adminList 返回账号列表与网关接入信息。
func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) {
	creds := h.pool.List()
	rows := make([]account.Snapshot, 0, len(creds))
	for _, c := range creds {
		rows = append(rows, c.Snapshot())
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accounts": rows,
		"config": map[string]any{
			"listen":      h.cfg.Listen,
			"api_key":     h.cfg.APIKey,
			"api_key_set": h.cfg.APIKey != "",
			"base_url":    h.baseURL(r),
			"account_dir": h.cfg.AccountDir,
			"pool": map[string]any{
				"strategy":      h.cfg.Pool.Strategy,
				"max_in_flight": h.cfg.Pool.MaxInFlight,
			},
			"session": map[string]any{
				"enabled":     h.cfg.Session.Enabled,
				"ttl_minutes": h.cfg.Session.TTLMinutes,
			},
		},
	})
}

// adminAdd 新增账号凭证：写入 accounts/<name>.json 并加入池。
func (h *Handler) adminAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string `json:"name"`
		SessionToken    string `json:"session_token"`
		CookieExpiresAt int64  `json:"cookie_expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "JSON 解析失败: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.SessionToken = strings.TrimSpace(req.SessionToken)
	if req.SessionToken == "" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "session_token 不能为空")
		return
	}

	// 未指定名字时用 token 前缀派生，保证文件名合法且唯一
	if req.Name == "" {
		req.Name = "account-" + req.SessionToken
		if len(req.Name) > 20 {
			req.Name = req.Name[:20]
		}
	}
	// 统一为账号 stem：去掉可能带的 .json 后缀，再过滤非法字符。
	// 允许 '.' 会导致生成 name.json.json，故这里只留字母数字与 -_。
	req.Name = strings.TrimSuffix(req.Name, ".json")
	req.Name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, req.Name)
	if req.Name == "" {
		req.Name = "account"
	}

	path := h.credPath(req.Name)
	c := &account.Cred{
		SessionToken:    req.SessionToken,
		CookieExpiresAt: req.CookieExpiresAt,
		FilePath:        path,
	}
	if err := c.SaveAtomic(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "write_failed", "写入凭证失败: "+err.Error())
		return
	}
	h.pool.Add(c)

	// 后台预热，补齐身份/工作区/agent/余额（不阻塞响应）
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		h.warmupCred(ctx, c)
	}()

	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": req.Name})
}

// adminAccountAction 处理 /admin/api/accounts/{name}/{action} 与删除。
func (h *Handler) adminAccountAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/accounts/")
	if rest == "" {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "缺少账号名")
		return
	}

	name := rest
	action := ""
	if i := strings.LastIndex(rest, "/"); i >= 0 {
		name, action = rest[:i], rest[i+1:]
	}
	if name == "" {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "缺少账号名")
		return
	}

	// name 允许带或不带 .json 后缀；统一成 stem 再定位文件。
	name = strings.TrimSuffix(name, ".json")
	c := h.findCred(name)
	if c == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "账号不存在: "+name)
		return
	}
	// 后续一律用凭证自带的真实路径，不再靠 name 重新拼，避免路径不一致。
	path := c.Path()

	switch {
	case action == "enable" && r.Method == http.MethodPost:
		c.SetDisabled(false, "")
		_ = c.SaveAtomic()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case action == "disable" && r.Method == http.MethodPost:
		c.SetDisabled(true, "manual")
		_ = c.SaveAtomic()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case action == "refresh" && r.Method == http.MethodPost:
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			h.warmupCred(ctx, c)
		}()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case action == "" && r.Method == http.MethodDelete:
		h.pool.Remove(path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			writeOpenAIError(w, http.StatusInternalServerError, "delete_failed", "删除文件失败: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"不支持的方法或动作: "+r.Method+" "+action)
	}
}

// findCred 按账号名查找凭证：先精确匹配路径，再回退到按文件 stem 匹配。
// 回退是为了兼容历史凭证文件名（例如手工创建时带了不规范的点号）。
func (h *Handler) findCred(name string) *account.Cred {
	if c := h.pool.Find(h.credPath(name)); c != nil {
		return c
	}
	for _, c := range h.pool.List() {
		p := c.Path()
		if p == "" {
			continue
		}
		if strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)) == name {
			return c
		}
	}
	return nil
}

// credPath 由账号 stem 拼出凭证文件的绝对/相对路径。
func (h *Handler) credPath(name string) string {
	return filepath.Join(h.cfg.AccountDir, name+".json")
}

// baseURL 拼出客户端访问网关时用的对外地址。
func (h *Handler) baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if host := r.Header.Get("X-Forwarded-Proto"); host != "" {
		scheme = host
	}
	hostname := r.Host
	if xf := r.Header.Get("X-Forwarded-Host"); xf != "" {
		hostname = xf
	}
	return scheme + "://" + hostname
}

// writeJSON 输出管理接口的通用 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
