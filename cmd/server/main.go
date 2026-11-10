// Command server 启动 sumus2api 网关：加载账号、初始化上游客户端、提供 OpenAI 兼容接口。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sumus2api/internal/account"
	"sumus2api/internal/config"
	"sumus2api/internal/proxy"
	"sumus2api/internal/upstream"
)

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	if err := os.MkdirAll(cfg.AccountDir, 0o755); err != nil {
		log.Fatalf("创建账号目录失败: %v", err)
	}

	store := account.NewStore()
	creds, err := account.LoadDir(cfg.AccountDir)
	if err != nil {
		log.Fatalf("加载账号失败: %v", err)
	}
	store.Replace(creds)

	if len(creds) == 0 {
		log.Printf("警告: 账号目录 %s 下没有可用凭证，网关将无法工作", cfg.AccountDir)
		log.Printf("请把 Sumus cookie 写入 %s/<name>.json，格式见 README", cfg.AccountDir)
	}

	client := upstream.New(
		cfg.Upstream.BaseURL,
		cfg.Upstream.ClientVersion,
		cfg.Upstream.ClientStage,
		cfg.Upstream.ClientLocale,
		cfg.Upstream.ClientTimezone,
		time.Duration(cfg.Upstream.TimeoutSeconds)*time.Second,
		cfg.Upstream.PollIntervalMs,
		cfg.Upstream.PollTimeoutSeconds,
	)

	// 启动时预热：拉取身份/余额/工作区/默认 agent
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	for _, c := range creds {
		if err := warmup(ctx, client, c); err != nil {
			log.Printf("账号预热失败 %s: %v", c.Email, err)
		}
	}
	cancel()

	pool := upstream.NewPool(store, cfg.Pool.MaxInFlight, cfg.Pool.Strategy)
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	pool.StartGC(bgCtx, 5*time.Minute)

	handler := proxy.NewHandler(cfg, client, pool)

	addr := cfg.Listen
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	// 优雅退出
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("收到退出信号，正在关闭...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("sumus2api 监听 %s", addr)
	log.Printf("账号数: %d", len(creds))
	if cfg.APIKey != "" {
		log.Printf("API key: %s", cfg.APIKey)
	} else {
		log.Printf("警告: 未设置 api_key，网关无鉴权，仅限本机使用")
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP 服务退出: %v", err)
	}
}

// warmup 拉取账号身份、工作区与余额，补齐凭证字段。
func warmup(ctx context.Context, client *upstream.Client, c *account.Cred) error {
	me, err := client.FetchMe(ctx, c.SessionToken)
	if err != nil {
		return fmt.Errorf("拉取身份: %w", err)
	}
	c.Lock()
	c.UserID = me.ID
	c.Email = me.Email
	c.Name = me.Name
	c.Unlock()

	// 拉工作区与默认 agent（首账号信息即可）
	bs, err := client.FetchBalance(ctx, c.SessionToken, "")
	if err == nil {
		c.Lock()
		c.Balance = parseInt64(bs.Balance)
		c.Unlock()
	}

	// 用 shell 视图拿 workspace 与 agents
	// 简化：直接用已知 workspace 端点
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

// parseInt64 宽松解析整数字符串。
func parseInt64(s string) int64 {
	var n int64
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}
