// Package config 负责加载与校验 sumus2api 的运行时配置。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config 是网关的完整运行时配置。
type Config struct {
	// Listen 是 HTTP 监听地址，例如 ":7864"。
	Listen string `json:"listen"`
	// APIKey 是网关自身的鉴权密钥。空字符串表示不鉴权（仅限本机调试）。
	APIKey string `json:"api_key"`
	// AccountDir 存放账号凭证文件（每账号一个 JSON）。
	AccountDir string `json:"account_dir"`
	// StateFile 是会话粘性等状态落盘路径。
	StateFile string `json:"state_file"`

	Upstream Upstream `json:"upstream"`
	Pool     Pool     `json:"pool"`
	Session  Session  `json:"session"`
}

// Upstream 描述 Sumus 上游的连接参数。
type Upstream struct {
	// BaseURL 固定为 https://sumus.im，仅用于测试时覆盖。
	BaseURL string `json:"base_url"`
	// ClientVersion 对应请求头 X-Client-Version。
	ClientVersion string `json:"client_version"`
	// ClientStage 对应请求头 X-Client-Stage。
	ClientStage string `json:"client_stage"`
	// ClientLocale 对应请求头 X-Client-Locale。
	ClientLocale string `json:"client_locale"`
	// ClientTimezone 对应请求头 X-Client-Timezone。
	ClientTimezone string `json:"client_timezone"`
	// TimeoutSeconds 是单次上游请求的总时长上限。
	TimeoutSeconds int `json:"timeout_seconds"`
	// PollIntervalMs 是轮询消息结果时的间隔。
	PollIntervalMs int `json:"poll_interval_ms"`
	// PollTimeoutSeconds 是等待 agent 回复的最长轮询时间。
	PollTimeoutSeconds int `json:"poll_timeout_seconds"`
}

// Pool 描述账号池的调度策略。
type Pool struct {
	// Strategy 支持 round_robin / weighted_random / sequential_exhaust。
	Strategy string `json:"strategy"`
	// MaxInFlight 是单账号最大在途请求数，0 表示不限。
	MaxInFlight int `json:"max_in_flight"`
}

// Session 描述会话粘性。
type Session struct {
	// Enabled 控制是否把同一 conversation_id 绑定到同一账号。
	Enabled bool `json:"enabled"`
	// TTLMinutes 是绑定存活时长。
	TTLMinutes int `json:"ttl_minutes"`
}

// Default 返回带推荐默认值的配置。
func Default() *Config {
	return &Config{
		Listen:     ":7864",
		AccountDir: "./accounts",
		StateFile:  "./data/state.json",
		Upstream: Upstream{
			BaseURL:            "https://sumus.im",
			ClientVersion:      "53888f788f5e",
			ClientStage:        "prod",
			ClientLocale:       "zh-CN",
			ClientTimezone:     "Asia/Shanghai",
			TimeoutSeconds:     60,
			PollIntervalMs:     1200,
			PollTimeoutSeconds: 300,
		},
		Pool: Pool{
			Strategy:    "weighted_random",
			MaxInFlight: 3,
		},
		Session: Session{
			Enabled:    true,
			TTLMinutes: 30,
		},
	}
}

// Load 读取配置文件；文件不存在时写入默认配置并返回。
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取配置 %s: %w", path, err)
		}
		// 首启：自动生成随机 api_key 并落盘
		cfg.APIKey = randomKey()
		if err := cfg.Save(path); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

// applyDefaults 补齐零值字段，避免用户配置缺项导致空指针或除零。
func (c *Config) applyDefaults() {
	d := Default()
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.AccountDir == "" {
		c.AccountDir = d.AccountDir
	}
	if c.StateFile == "" {
		c.StateFile = d.StateFile
	}
	if c.Upstream.BaseURL == "" {
		c.Upstream.BaseURL = d.Upstream.BaseURL
	}
	if c.Upstream.ClientVersion == "" {
		c.Upstream.ClientVersion = d.Upstream.ClientVersion
	}
	if c.Upstream.ClientStage == "" {
		c.Upstream.ClientStage = d.Upstream.ClientStage
	}
	if c.Upstream.ClientLocale == "" {
		c.Upstream.ClientLocale = d.Upstream.ClientLocale
	}
	if c.Upstream.ClientTimezone == "" {
		c.Upstream.ClientTimezone = d.Upstream.ClientTimezone
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = d.Upstream.TimeoutSeconds
	}
	if c.Upstream.PollIntervalMs <= 0 {
		c.Upstream.PollIntervalMs = d.Upstream.PollIntervalMs
	}
	if c.Upstream.PollTimeoutSeconds <= 0 {
		c.Upstream.PollTimeoutSeconds = d.Upstream.PollTimeoutSeconds
	}
	if c.Pool.Strategy == "" {
		c.Pool.Strategy = d.Pool.Strategy
	}
	if c.Session.TTLMinutes <= 0 {
		c.Session.TTLMinutes = d.Session.TTLMinutes
	}
}

// Save 把配置原子写入 path。
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// randomKey 生成 24 字节随机十六进制串作为默认 API key。
func randomKey() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// 极小概率失败时退化为时间戳，仍保证非空
		return fmt.Sprintf("k%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
