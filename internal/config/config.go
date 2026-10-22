package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go-codearts/internal/kit"
)

// Config is loaded from environment variables.
type Config struct {
	Port              string
	AdminUser         string
	AdminPassword     string
	DataDir           string
	PublicURL         string
	BasePath          string // e.g. /codearts when reverse-proxied under an existing domain
	CaptchaSolverURL  string
	RegisterAreaCode  string
	OutboundProxy     string
	OAuthCallbackPath string
	APIKey            string
	HuaweiModelBase   string
	HuaweiModelsURL   string
	QuotaSyncInterval time.Duration
	MaxRequestBytes   int

	// Scheduler (Track 1/2). Zero-value Server in tests keeps legacy rotate=2.
	RotateByGate             bool
	RotateAfterReady         int
	RotateAfterDepleted      int
	HeaderWaitDepleted       time.Duration
	NonStreamTimeoutDepleted time.Duration
	EarlyEnvelope            bool // Track 1a: message_start / response.created before ping

	// Per-API-key fairness (Track 3). 0 = disabled.
	KeyRPM         int
	KeyMaxInFlight int

	// HuaweiDispatchRPS paces outbound Huawei chat dispatches process-wide
	// (not per-account/per-key — see dispatch_limiter.go). Huawei's own error
	// states "the rate limit is 50 per second" as an aggregate ceiling shared
	// across the whole account pool; default leaves ~10% margin. 0 = disabled.
	HuaweiDispatchRPS int

	// ForceThinking forces thinking injection on every tool-bearing request.
	// Default off — needsThinking only repairs assistant tool_calls missing
	// reasoning_content. On historically caused 30-90s GLM reasoning per request.
	ForceThinking bool

	// SSEIdleTimeout caps upstream SSE idle gap. 0 = default (600s).
	// Long tasks (hours) need a wider window than the old 180s.
	SSEIdleTimeout time.Duration

	// CN channel (国内码道/盘古助手) — accounts imported from CN_AUTH_DIR auths/*.json.
	CNEnabled           bool
	CNAuthDir           string
	CNDefaultModel      string
	CNSoftCooldown      time.Duration // per-account soft cooldown between picks
	CNErrThreshold      int           // consecutive errors before cooldown kicks in
	CNErrCooldown       time.Duration
	CNBenefitAutoClaim  bool // auto-claim limited-time model benefits on discovery
	CNCreditsAutoClaim  bool // watchdog daily checkin / newbie credits auto-claim
	CNMaxConcurrent     int  // per-CN-account max in-flight chat requests
	CNKeepaliveWindow   time.Duration
	CNKeepaliveInterval time.Duration
	CNWatchPoll         time.Duration // watchdog poll cycle
	CNRefreshSkew       time.Duration // refresh token this early before expiration
	CNUpstreamTimeout   time.Duration
	// CNOAuthCallbackHost 可选：反向代理暴露的公网回调入口（如
	// https://panel.example.com:443）。只有显式写了端口才改写授权链接里的
	// port 参数；不写端口保持本机 127.0.0.1 回调不变。
	CNOAuthCallbackHost string

	// RespCacheTTL enables the local identical-body response cache. Default 5m.
	// Covers both streaming and non-streaming: identical sanitized request bodies
	// get a cached OpenAI completion replayed in the client's protocol (OpenAI SSE,
	// Claude Messages, or Responses). Catches client retries and duplicate sends.
	// 0 = disabled.
	RespCacheTTL time.Duration
}

// ReloadEnv rereads a .env file into the process environment. The set of
// KEY=VALUE lines is applied via os.Setenv, overriding stale exports.
// Meant for hot-reload wired to the fs watcher — pair with a fresh Get() plus
// a whitelist swap on the running Server (only fields safe to change live).
// Path defaults to ".env" (matches boot behavior).
func ReloadEnv(path string) {
	if strings.TrimSpace(path) == "" {
		path = ".env"
	}
	loadDotEnv(path)
}

func Get() Config {
	loadDotEnv(".env")
	cfg := Config{
		Port:                     envOr("PORT", "29137"),
		AdminUser:                envOr("ADMIN_USERNAME", "admin"),
		AdminPassword:            envOr("ADMIN_PASSWORD", "admin123"),
		DataDir:                  envOr("DATA_DIR", "./data"),
		PublicURL:                strings.TrimRight(envOr("PUBLIC_URL", "http://localhost:29137"), "/"),
		BasePath:                 normalizeBasePath(os.Getenv("BASE_PATH")),
		CaptchaSolverURL:         os.Getenv("CAPTCHA_SOLVER_URL"),
		RegisterAreaCode:         envOr("REG_AREA_CODE", "OM"),
		OutboundProxy:            os.Getenv("HTTPS_PROXY"),
		OAuthCallbackPath:        "/oauth/callback",
		APIKey:                   os.Getenv("API_KEY"),
		HuaweiModelBase:          envOr("HUAWEI_MODEL_BASE", kit.X("sIvnmE0RMsrTyEWYhnq7QxFrB1MPWHJBUKvnm496uaEYI6w7DFt5A1OpRPquu3mjeGmEOw==")),
		HuaweiModelsURL:          os.Getenv("HUAWEI_MODELS_URL"),
		QuotaSyncInterval:        envDuration("QUOTA_SYNC_INTERVAL", 30*time.Minute),
		RotateByGate:             envBool("ROTATE_BY_GATE", true),
		RotateAfterReady:         envInt("ROTATE_AFTER_READY", 5),
		RotateAfterDepleted:      envInt("ROTATE_AFTER_DEPLETED", 2),
		HeaderWaitDepleted:       envDuration("HEADER_WAIT_DEPLETED", 40*time.Second),
		NonStreamTimeoutDepleted: envDuration("NONSTREAM_TIMEOUT_DEPLETED", 45*time.Second),
		EarlyEnvelope:            envBool("EARLY_ENVELOPE", true),
		KeyRPM:                   envInt("KEY_RPM", 0),
		KeyMaxInFlight:           envInt("KEY_MAX_INFLIGHT", 8),
		HuaweiDispatchRPS:        envInt("HUAWEI_DISPATCH_RPS", 45),
		ForceThinking:            envBool("FORCE_THINKING", false),
		SSEIdleTimeout:           envDuration("SSE_IDLE_TIMEOUT", 600*time.Second),
		RespCacheTTL:             envDuration("RESP_CACHE_TTL", 5*time.Minute),

		CNEnabled:           envBool("CN_ENABLED", true),
		CNAuthDir:           envOr("CN_AUTH_DIR", "./auths"),
		CNDefaultModel:      envOr("CN_DEFAULT_MODEL", "snap-chat"),
		CNSoftCooldown:      envDuration("CN_SOFT_COOLDOWN", 60*time.Second),
		CNErrThreshold:      envInt("CN_ERR_THRESHOLD", 3),
		CNErrCooldown:       envDuration("CN_ERR_COOLDOWN", 10*time.Minute),
		CNBenefitAutoClaim:  envBool("CN_BENEFIT_AUTO_CLAIM", true),
		CNCreditsAutoClaim:  envBool("CN_CREDITS_AUTO_CLAIM", true),
		CNMaxConcurrent:     envInt("CN_MAX_CONCURRENT", 5),
		CNKeepaliveWindow:   envDuration("CN_KEEPALIVE_WINDOW", 10*time.Minute),
		CNKeepaliveInterval: envDuration("CN_KEEPALIVE_INTERVAL", 15*time.Minute),
		CNWatchPoll:         envDuration("CN_WATCH_POLL", 30*time.Minute),
		CNRefreshSkew:       envDuration("CN_REFRESH_SKEW", 30*time.Minute),
		CNUpstreamTimeout:   envDuration("CN_UPSTREAM_TIMEOUT", 120*time.Second),
		CNOAuthCallbackHost: envOr("CN_OAUTH_CALLBACK_HOST", ""),
	}
	if cfg.PublicURL == "" {
		cfg.PublicURL = "http://localhost:" + cfg.Port
	}
	return cfg
}

func normalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

func envOr(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n := 0
	neg := false
	for i, c := range v {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// loadDotEnv applies KEY=VALUE from a local .env file, overriding process env
// so a stale shell export cannot pin the wrong region.
func loadDotEnv(name string) {
	f, err := os.Open(name)
	if err != nil {
		if wd, err2 := os.Getwd(); err2 == nil {
			f, err = os.Open(filepath.Join(wd, name))
		}
	}
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		if k != "" {
			_ = os.Setenv(k, v)
		}
	}
}
