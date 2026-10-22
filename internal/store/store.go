package store

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type DpopKeyPair struct {
	PrivateKeyJwk string `json:"private_key_jwk"`
	PublicKeyJwk  string `json:"public_key_jwk"`
}

type Credentials struct {
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	SecurityToken   string    `json:"security_token"`
	Expiration      time.Time `json:"expiration"`
	// AuthMode: iam_permanent = 官方 CLI 永久 AK/SK；oauth_sts = OAuth 临时票
	AuthMode    string       `json:"auth_mode,omitempty"`
	DpopKeyPair *DpopKeyPair `json:"dpop_key_pair,omitempty"`
}

// Usable reports whether credentials can sign CodeArts APIs.
// Permanent IAM AK/SK (official CLI path) have no security_token and no expiry.
func (c *Credentials) Usable() bool {
	if c == nil || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return false
	}
	if c.AuthMode == "iam_permanent" || c.SecurityToken == "" {
		return true
	}
	return c.Expiration.IsZero() || c.Expiration.After(time.Now())
}

// IsPermanent is the official CLI授权指导 path (CODEARTS_CLI_AK/SK).
func (c *Credentials) IsPermanent() bool {
	if c == nil {
		return false
	}
	return c.AuthMode == "iam_permanent" || (c.AccessKeyID != "" && c.SecurityToken == "")
}

// QuotaSnapshot is cached account quota for the dashboard.
type QuotaSnapshot struct {
	PackageStatus string `json:"package_status"`
	PackageName   string `json:"package_name"`
	ExpireTime    string `json:"expire_time"`
	EffectiveTime string `json:"effective_time"`
	// Real upstream validity window (payload start_date / end_date).
	StartDate      string  `json:"start_date,omitempty"`
	EndDate        string  `json:"end_date,omitempty"`
	UsedTokens     float64 `json:"used_tokens"`
	PackageTokens  float64 `json:"package_tokens"`
	RemainTokens   float64 `json:"remain_tokens"`
	UsagePercent   float64 `json:"usage_percent"`
	MetricName     string  `json:"metric_name"`
	Period         string  `json:"period"`
	PeriodLabel    string  `json:"period_label"`
	PeriodEvidence string  `json:"period_evidence,omitempty"`
	// Upstream-owned reset schedule: explicit upstream field or labelled estimate.
	// omitzero (Go 1.24+) so stale snapshots don't ship "0001-01-01T00:00:00Z".
	NextResetAt *time.Time `json:"next_reset_at,omitempty"`
	ResetSource string     `json:"reset_source,omitempty"`
	ResetNote   string     `json:"reset_note,omitempty"`
	// Observed reset history: upstream owns the moment, so we record counter
	// rollbacks seen during quota refresh (used drop / budget refill).
	LastResetAt        *time.Time `json:"last_reset_at,omitempty"`
	ResetCount         int        `json:"reset_count,omitempty"`
	ResetIntervalHours float64    `json:"reset_interval_hours,omitempty"`
	FetchedAt          time.Time  `json:"fetched_at"`
	Error              string     `json:"error,omitempty"`
}

// Call gate values.
// Ready = normal budget; QuotaExhausted = 透支中 (still callable, lower pick weight).
// Only Disabled / Expired / NoCred / TokenExpired are hard-blocked from chat.
const (
	CallGateReady          = "ready"
	CallGateQuotaExhausted = "quota_exhausted" // 透支中：本地额度耗尽但仍可调上游
	CallGateExpired        = "expired"
	CallGateDisabled       = "disabled"
	CallGateNoCred         = "no_cred"
	CallGateTokenExpired   = "token_expired"
	CallGateUnknown        = "unknown"

	// Pick weights for round-robin expansion (ready : depleted).
	PickWeightReady    = 4
	PickWeightDepleted = 1
)

// CallGateLabelZH returns a short Chinese badge (≤3 runes where possible).
func CallGateLabelZH(gate string) string {
	switch gate {
	case CallGateReady:
		return "可调用"
	case CallGateQuotaExhausted:
		return "透支中"
	case CallGateExpired:
		return "已到期"
	case CallGateDisabled:
		return "已禁用"
	case CallGateNoCred:
		return "无凭证"
	case CallGateTokenExpired:
		return "票过期"
	default:
		return "未知"
	}
}

// NormalizeRemain fills remain_tokens when upstream only returns package/used.
func (q *QuotaSnapshot) NormalizeRemain() {
	if q == nil {
		return
	}
	if q.RemainTokens <= 0 && q.PackageTokens > 0 {
		q.RemainTokens = q.PackageTokens - q.UsedTokens
		if q.RemainTokens < 0 {
			q.RemainTokens = 0
		}
	}
	if q.UsagePercent <= 0 && q.PackageTokens > 0 {
		q.UsagePercent = q.UsedTokens / q.PackageTokens * 100
	}
}

// HasQuotaBudget reports whether cached quota still has remaining tokens.
// If package_tokens is unknown (0) and we never fetched, treat as unknown (true)
// so brand-new accounts are not soft-banned before first quota sync.
func (q *QuotaSnapshot) HasQuotaBudget() bool {
	if q == nil {
		return true
	}
	if q.Error != "" && q.PackageTokens <= 0 && q.UsedTokens <= 0 && q.RemainTokens <= 0 {
		return true
	}
	q.NormalizeRemain()
	if q.PackageTokens > 0 {
		return q.RemainTokens > 0
	}
	// No package size known: if remain explicitly 0 after a successful fetch, block.
	if !q.FetchedAt.IsZero() && q.Error == "" && q.RemainTokens <= 0 && q.UsedTokens > 0 {
		return false
	}
	return true
}

// PackageLooksExpired uses upstream package_status / expire_time when present.
func (q *QuotaSnapshot) PackageLooksExpired(now time.Time) bool {
	if q == nil {
		return false
	}
	st := strings.ToLower(strings.TrimSpace(q.PackageStatus))
	if st == "expired" || st == "expire" || st == "invalid" || st == "closed" || st == "terminated" {
		return true
	}
	if t := ParseFlexibleTime(q.ExpireTime); !t.IsZero() && t.Before(now) {
		return true
	}
	return false
}

type Account struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	// Channel identifies the account source: "intl" = default stock (国际版),
	// "cn" = 国内码道渠道. Empty legacy rows behave as intl via NormalizeChannel.
	Channel string `json:"channel,omitempty"`
	// CN-channel credentials (国内码道, imported from codearts2api auths/*.json).
	// UserID prefixes nothing extra; AccessToken is the cloud_dragon_token used
	// as x-auth-token upstream; ChatID pins the 会话续接 thread.
	UserID          string            `json:"user_id,omitempty"`
	AccessToken     string            `json:"access_token,omitempty"`
	ChatID          string            `json:"chat_id,omitempty"`
	Password        string            `json:"password,omitempty"`
	CountryCode     string            `json:"country_code"`
	Status          string            `json:"status"`
	Opened          bool              `json:"opened"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Credentials     *Credentials      `json:"credentials,omitempty"`
	RefreshToken    string            `json:"refresh_token,omitempty"`
	TokenExpires    time.Time         `json:"token_expires_at,omitempty"`
	UserInfo        map[string]any    `json:"user_info,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	Models          []string          `json:"models,omitempty"`
	ModelsFetchedAt time.Time         `json:"models_fetched_at,omitempty"`
	ModelsError     string            `json:"models_error,omitempty"`
	Quota           *QuotaSnapshot    `json:"quota,omitempty"`
	ProxyID         string            `json:"proxy_id,omitempty"`

	// CallGate is recomputed from credentials + quota + Disabled (not a free-form status).
	CallGate string `json:"call_gate,omitempty"`
	// Disabled is manual admin switch; independent from quota soft-ban.
	Disabled       bool      `json:"disabled,omitempty"`
	DisabledReason string    `json:"disabled_reason,omitempty"` // manual | quota_exhausted | upstream_rejected | …
	QuotaBlockedAt time.Time `json:"quota_blocked_at,omitempty"`
	LastUsedAt     time.Time `json:"last_used_at,omitempty"`
	LastSuccessAt  time.Time `json:"last_success_at,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	SuccessCount   int64     `json:"success_count,omitempty"`
	FailCount      int64     `json:"fail_count,omitempty"`
	StickHits      int64     `json:"stick_hits,omitempty"`
	CachedTokens   float64   `json:"cached_tokens,omitempty"` // upstream prompt-cache tokens observed
	// Per-account token/latency stats for dashboard快览.
	PromptTokens     float64 `json:"prompt_tokens_total,omitempty"`
	CompletionTokens float64 `json:"completion_tokens_total,omitempty"`
	LastLatencyMS    int64   `json:"last_latency_ms,omitempty"`
	LastModel        string  `json:"last_model,omitempty"`
	// Live fields filled by API (not meaningful in persisted snapshot alone).
	StickSessions    int    `json:"stick_sessions,omitempty"`
	CacheStatus      string `json:"cache_status,omitempty"` // none|sticky|warm
	CacheStatusLabel string `json:"cache_status_label,omitempty"`
	Circuit          int    `json:"circuit,omitempty"` // 0 closed / 1 open / 2 half-open; API-filled
	Notes            string `json:"notes,omitempty"`
	// Priority tier for picker preference. Lower = higher priority. Default 0
	// means "standard tier". Positive values sink an account (backup only),
	// negative values pin it as premium. Ties within a tier fall through to
	// the existing load/stick/recency scorer.
	Priority int `json:"priority,omitempty"`
}

// RecomputeCallGate updates CallGate / quota soft-ban fields from current state.
// Manual Disabled always wins. Does not invent expire times.
func (a *Account) RecomputeCallGate(now time.Time) {
	if a == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if a.Disabled {
		a.CallGate = CallGateDisabled
		if a.DisabledReason == "" {
			a.DisabledReason = "manual"
		}
		return
	}
	// CN 渠道（国内码道）走 OAuth：只需 access_token（cloud_dragon_token），
	// 不依赖 IAM AK/SK。若仍套 Credentials 判据，纯 OAuth 导入的账号会永远
	// no_cred → 整个国内渠道在调度池里恒空。
	if NormalizeChannel(a.Channel) == ChannelCN {
		if strings.TrimSpace(a.AccessToken) == "" {
			a.CallGate = CallGateNoCred
			return
		}
		// token 过期：有 refresh_token 就放行到 ready（chat 侧会 inline 续期，
		// cnwatch 也会按 skew 兜底），否则判过期让运维重登。
		if !a.TokenExpires.IsZero() && a.TokenExpires.Before(now) && a.RefreshToken == "" {
			a.CallGate = CallGateTokenExpired
			return
		}
		if a.Quota != nil && a.Quota.PackageLooksExpired(now) {
			a.CallGate = CallGateExpired
			return
		}
		if a.Quota != nil && !a.Quota.HasQuotaBudget() {
			a.CallGate = CallGateQuotaExhausted
			a.DisabledReason = "quota_exhausted"
			if a.QuotaBlockedAt.IsZero() {
				a.QuotaBlockedAt = now
			}
			return
		}
		if a.DisabledReason == "quota_exhausted" || a.DisabledReason == "expired" || a.DisabledReason == "upstream_rejected" {
			a.DisabledReason = ""
			a.QuotaBlockedAt = time.Time{}
		}
		a.CallGate = CallGateReady
		return
	}
	if a.Credentials == nil || !a.Credentials.Usable() {
		a.CallGate = CallGateNoCred
		return
	}
	if !a.Credentials.IsPermanent() && !a.TokenExpires.IsZero() && a.TokenExpires.Before(now) {
		a.CallGate = CallGateTokenExpired
		return
	}
	if a.Quota != nil && a.Quota.PackageLooksExpired(now) {
		a.CallGate = CallGateExpired
		if a.DisabledReason == "" || a.DisabledReason == "quota_exhausted" {
			a.DisabledReason = "expired"
		}
		if a.QuotaBlockedAt.IsZero() {
			a.QuotaBlockedAt = now
		}
		return
	}
	if a.Quota != nil && !a.Quota.HasQuotaBudget() {
		a.CallGate = CallGateQuotaExhausted
		a.DisabledReason = "quota_exhausted"
		if a.QuotaBlockedAt.IsZero() {
			a.QuotaBlockedAt = now
		}
		return
	}
	// Recovered / ready.
	if a.DisabledReason == "quota_exhausted" || a.DisabledReason == "expired" || a.DisabledReason == "upstream_rejected" {
		a.DisabledReason = ""
		a.QuotaBlockedAt = time.Time{}
	}
	a.CallGate = CallGateReady
}

// Callable is true when this account may be used for chat completions.
// QuotaExhausted (透支中) stays callable; only hard gates are excluded.
func (a *Account) Callable(now time.Time) bool {
	if a == nil {
		return false
	}
	a.RecomputeCallGate(now)
	return a.CallGate == CallGateReady || a.CallGate == CallGateQuotaExhausted
}

// Depleted is true when local Free quota is exhausted but account remains callable.
func (a *Account) Depleted() bool {
	return a != nil && a.CallGate == CallGateQuotaExhausted
}

// PickWeight returns relative selection weight (ready ≫ 透支中).
func (a *Account) PickWeight() int {
	if a == nil {
		return 0
	}
	switch a.CallGate {
	case CallGateReady:
		return PickWeightReady
	case CallGateQuotaExhausted:
		return PickWeightDepleted
	default:
		return 0
	}
}

// MarkQuotaExhausted marks 透支中 (local remain=0). Does NOT hard-disable;
// account stays in the callable pool at lower weight.
func (a *Account) MarkQuotaExhausted(reason, errMsg string, now time.Time) {
	if a == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if reason == "" {
		reason = "quota_exhausted"
	}
	a.DisabledReason = reason
	a.QuotaBlockedAt = now
	a.CallGate = CallGateQuotaExhausted
	if errMsg != "" {
		a.LastError = errMsg
	}
	if a.Quota != nil {
		a.Quota.NormalizeRemain()
		if a.Quota.RemainTokens > 0 {
			a.Quota.RemainTokens = 0
		}
		if a.Quota.PackageTokens > 0 {
			a.Quota.UsedTokens = a.Quota.PackageTokens
			a.Quota.UsagePercent = 100
		}
	}
}

// MarkUpstreamRejected hard-bans after a clear upstream quota/auth refusal.
func (a *Account) MarkUpstreamRejected(errMsg string, now time.Time) {
	if a == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	a.Disabled = true
	a.DisabledReason = "upstream_rejected"
	a.QuotaBlockedAt = now
	a.CallGate = CallGateDisabled
	if errMsg != "" {
		a.LastError = errMsg
	}
}

// hwNaiveLoc 无时区时间串按华为上游语义固定 +08:00（CST）解析——
// 用容器/宿主机的本地时区会让 TokenExpires 错位 ±8h。
var hwNaiveLoc = time.FixedZone("CST", 8*3600)

// ParseFlexibleTime parses common Huawei time strings (shared with quota helpers).
// 带时区的布局按原值解析；无时区的布局一律按 UTC+8（华为侧是北京时间）。
func ParseFlexibleTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	zoned := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05Z07:00"}
	for _, layout := range zoned {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	naive := []string{"2006-01-02 15:04:05", "2006-01-02", "2006/01/02 15:04:05", "2006/01/02"}
	for _, layout := range naive {
		if t, err := time.ParseInLocation(layout, s, hwNaiveLoc); err == nil {
			return t
		}
	}
	return time.Time{}
}

type DB struct {
	mu       sync.Mutex
	path     string
	Accounts map[string]*Account `json:"accounts"`
	Version  int                 `json:"version"`

	// refreshLocks 按账号 ID 单飞互斥，防止 cnwatch 与 cnchat 双通道刷新
	// 同一个账号导致 refresh token 轮换 write-last-wins 自毁。
	refreshLocks map[string]*sync.Mutex

	// Debounced write: avoids writing the entire JSON file on every Upsert.
	// A background goroutine flushes dirty state every writeInterval.
	dirty         bool
	txtDirty      bool
	writeStop     chan struct{}
	writeDone     chan struct{}
	writeInterval time.Duration
	fieldCipher   *FieldCipher

	// lastWriteSum/hasLastWrite 记录自己最后一次落盘的内容哈希，
	// 让热重载能区分"外部编辑"与"自己的异步写"，避免自我回滚。
	lastWriteSum [32]byte
	hasLastWrite bool
}

const defaultWriteInterval = 2 * time.Second

func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	// Initialize field cipher for AK/SK/password encryption (BUG-4 fix).
	fieldCipher, err := newFieldCipher(dataDir)
	if err != nil {
		return nil, fmt.Errorf("initialize field cipher: %w", err)
	}
	path := filepath.Join(dataDir, "accounts.json")
	db := &DB{
		path:          path,
		Accounts:      map[string]*Account{},
		writeStop:     make(chan struct{}),
		writeDone:     make(chan struct{}),
		writeInterval: defaultWriteInterval,
		fieldCipher:   fieldCipher,
		refreshLocks:  map[string]*sync.Mutex{},
	}
	data, err := os.ReadFile(path)
	if err == nil {
		// 解析失败绝不能当成"空库"继续跑：下一次 Upsert 会用空 map 覆盖
		// 整个 accounts.json，全部凭证永久丢失。宁可起不来，并把坏文件
		// 挪到一边留给运维，也不要静默毁数据。
		if uerr := json.Unmarshal(data, db); uerr != nil {
			bad := path + ".corrupt-" + time.Now().Format("20060102-150405")
			_ = os.Rename(path, bad)
			return nil, fmt.Errorf("accounts.json 解析失败（已备份到 %s）: %w", bad, uerr)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if db.Accounts == nil {
		db.Accounts = map[string]*Account{}
	}
	// Decrypt sensitive fields after loading (BUG-4 fix).
	migrationNeeded := false
	for _, a := range db.Accounts {
		if a == nil {
			continue
		}
		if db.sensitiveNeedsMigration(a) {
			migrationNeeded = true
		}
		if err := db.decryptSensitive(a); err != nil {
			return nil, fmt.Errorf("decrypt sensitive fields for %s: %w", a.ID, err)
		}
	}
	// Rewrite legacy plaintext snapshots and credential exports after startup.
	if migrationNeeded {
		db.dirty = true
	}
	_ = db.rewriteAccountsTxtLocked()
	go db.debouncedWriter()
	return db, nil
}

// debouncedWriter flushes dirty state to disk periodically.
// This prevents every Upsert from triggering a full JSON write,
// which was a major I/O bottleneck under concurrent chat load.
func (db *DB) debouncedWriter() {
	t := time.NewTicker(db.writeInterval)
	defer t.Stop()
	for {
		select {
		case <-db.writeStop:
			// Final flush on shutdown
			db.mu.Lock()
			if db.dirty {
				_ = db.saveLocked()
				db.dirty = false
			}
			if db.txtDirty {
				_ = db.rewriteAccountsTxtLocked()
				db.txtDirty = false
			}
			db.mu.Unlock()
			close(db.writeDone)
			return
		case <-t.C:
			db.mu.Lock()
			if db.dirty {
				_ = db.saveLocked()
				db.dirty = false
			}
			if db.txtDirty {
				_ = db.rewriteAccountsTxtLocked()
				db.txtDirty = false
			}
			db.mu.Unlock()
		}
	}
}

func (db *DB) saveLocked() error {
	// Serialize an isolated snapshot. Never mutate live account pointers while
	// encrypting: chat/quota goroutines may read them concurrently.
	type diskSnapshot struct {
		Accounts map[string]*Account `json:"accounts"`
		Version  int                 `json:"version"`
	}
	plain, err := json.Marshal(&diskSnapshot{Accounts: db.Accounts, Version: db.Version})
	if err != nil {
		return err
	}
	var snapshot diskSnapshot
	if err := json.Unmarshal(plain, &snapshot); err != nil {
		return err
	}
	for _, a := range snapshot.Accounts {
		db.encryptSensitive(a)
	}
	data, err := json.MarshalIndent(&snapshot, "", "  ")
	if err != nil {
		return err
	}
	// 落盘必须扛住掉电：写临时文件 → fsync → 轮转 .bak → rename → fsync 目录。
	// 只做 tmp+rename 的话，掉电可能留下 0 字节的 accounts.json（rename 的
	// 元数据可能先于数据落盘），而那是全部凭证的唯一副本。
	tmp := db.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// 上一版留一份 .bak，损坏时还有一次回头路。
	if _, statErr := os.Stat(db.path); statErr == nil {
		_ = copyFile(db.path, db.path+".bak")
	}
	if err := os.Rename(tmp, db.path); err != nil {
		os.Remove(tmp)
		return err
	}
	// rename 本身也是目录项变更，目录 fsync 之后才算真正落地。
	if dir, derr := os.Open(filepath.Dir(db.path)); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	// 记住自己写出去的内容：热重载 watcher 看到 mtime 变化时，
	// 用它区分"外部手工编辑"与"自己的异步落盘"，避免自我回滚。
	db.lastWriteSum = sha256.Sum256(data)
	db.hasLastWrite = true
	return nil
}

// copyFile 尽力拷贝（备份失败不阻断主流程）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ReloadFromDisk re-reads accounts.json and atomically swaps the in-memory
// map. Used by the fsnotify/mtime watcher when the operator hand-edits the
// file. Partial/invalid JSON is REJECTED — the live map is preserved so a
// half-written file never corrupts state. Sensitive fields are decrypted in
// the temp map before swap, matching Open() semantics.
func (db *DB) ReloadFromDisk() error {
	data, err := os.ReadFile(db.path)
	if err != nil {
		return err
	}
	// 自写抑制：saveLocked 每 2 秒可能落一次盘，watcher 会看到 mtime 变化。
	// 若内容与自己刚写的一致就直接返回，否则热重载会用磁盘快照整体替换内存，
	// 把这两秒内新产生的续期 token/签到标记/禁用状态全部回滚掉。
	sum := sha256.Sum256(data)
	db.mu.Lock()
	same := db.hasLastWrite && sum == db.lastWriteSum
	db.mu.Unlock()
	if same {
		return nil
	}
	// Decode into a throwaway struct — never touch live db.Accounts on parse err.
	type diskSnapshot struct {
		Accounts map[string]*Account `json:"accounts"`
		Version  int                 `json:"version"`
	}
	var snap diskSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	if snap.Accounts == nil {
		snap.Accounts = map[string]*Account{}
	}
	for _, a := range snap.Accounts {
		if a == nil {
			continue
		}
		if err := db.decryptSensitive(a); err != nil {
			return fmt.Errorf("decrypt on reload for %s: %w", a.ID, err)
		}
	}
	db.mu.Lock()
	db.Accounts = snap.Accounts
	db.Version = snap.Version
	// The reload IS canonical — don't mark dirty and clobber it back.
	db.mu.Unlock()
	return nil
}

func (db *DB) List() []*Account {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]*Account, 0, len(db.Accounts))
	for _, a := range db.Accounts {
		if a == nil {
			continue
		}
		out = append(out, deepCopyAccount(a))
	}
	return out
}

// Get 返回账号副本。调用方拿到的指针与库内状态解耦：并发写盘时
// json.Marshal 会遍历库内 map，若把活指针交出去、调用方在锁外改字段，
// 就是 Go 运行时的 fatal concurrent map read/write（recover 拦不住）。
// 要改账号一律走 Mutate。
func (db *DB) Get(id string) (*Account, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	a, ok := db.Accounts[id]
	if !ok || a == nil {
		return nil, errors.New("account not found")
	}
	return deepCopyAccount(a), nil
}

// FindByEmail returns the first account matching email (case-insensitive).
func (db *DB) FindByEmail(email string) *Account {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, a := range db.Accounts {
		if a != nil && strings.ToLower(strings.TrimSpace(a.Email)) == email {
			return deepCopyAccount(a)
		}
	}
	return nil
}

// CredentialLine is one imported / exported txt row.
type CredentialLine struct {
	Email    string
	Password string
	AK       string
	SK       string
	Opened   *bool // nil = unknown
	Raw      string
}

// ParseCredentialLine accepts:
//
//	email----password----ak----sk
//	email----password----开通:是/否
//	email---password---ak/sk   (legacy create_aksk)
func ParseCredentialLine(raw string) (CredentialLine, bool) {
	raw = strings.TrimSpace(raw)
	out := CredentialLine{Raw: raw}
	if raw == "" || strings.HasPrefix(raw, "#") {
		return out, false
	}
	// Strip trailing timestamp: ......YYYY-MM-DD ...
	if i := strings.LastIndex(raw, "------"); i > 0 {
		raw = strings.TrimSpace(raw[:i])
	}

	if parts := strings.Split(raw, "----"); len(parts) >= 4 {
		out.Email = strings.TrimSpace(parts[0])
		out.Password = strings.TrimSpace(parts[1])
		out.AK = strings.TrimSpace(parts[2])
		out.SK = strings.TrimSpace(parts[3])
		if out.Email == "" || out.AK == "" || out.SK == "" {
			return out, false
		}
		if strings.HasPrefix(out.AK, "开通:") {
			return out, false
		}
		opened := true
		out.Opened = &opened
		return out, true
	}
	if parts := strings.Split(raw, "----"); len(parts) == 3 {
		out.Email = strings.TrimSpace(parts[0])
		out.Password = strings.TrimSpace(parts[1])
		flag := strings.TrimSpace(parts[2])
		if out.Email == "" {
			return out, false
		}
		if strings.HasPrefix(flag, "开通:") {
			v := strings.TrimPrefix(flag, "开通:")
			opened := v == "是" || strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes")
			out.Opened = &opened
			return out, true
		}
		// email----password----ak/sk
		if ak, sk, ok := splitAKSK(flag); ok {
			out.AK, out.SK = ak, sk
			opened := true
			out.Opened = &opened
			return out, true
		}
		return out, false
	}
	// legacy: email---password---ak/sk
	if parts := strings.Split(raw, "---"); len(parts) == 3 {
		out.Email = strings.TrimSpace(parts[0])
		out.Password = strings.TrimSpace(parts[1])
		if ak, sk, ok := splitAKSK(strings.TrimSpace(parts[2])); ok && out.Email != "" {
			out.AK, out.SK = ak, sk
			opened := true
			out.Opened = &opened
			return out, true
		}
	}
	return out, false
}

func splitAKSK(s string) (ak, sk string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if i := strings.Index(s, "/"); i > 0 && i < len(s)-1 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
	}
	return "", "", false
}

func (db *DB) Upsert(a *Account) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	a.UpdatedAt = time.Now()
	a.RecomputeCallGate(a.UpdatedAt)
	db.Accounts[a.ID] = a
	// Mark dirty instead of writing immediately.
	// The debouncedWriter goroutine will flush within writeInterval.
	db.dirty = true
	// accounts.txt sync is debounced too — Upsert fires on every chat
	// completion (success/fail/rotate), and a synchronous os.WriteFile here
	// while holding db.mu serialized db.List() (called on every account pick)
	// behind disk I/O under concurrent chat load. Content rarely even changes
	// per chat Upsert (only email/opened are written), so the immediate write
	// was pure overhead on the hot path.
	db.txtDirty = true
	return nil
}

// MarkOAuthPending atomically marks an account as oauth_pending and wipes
// credentials under db.mu. This prevents chat goroutines from racing on
// acc.Credentials while it's being nil-ed (BUG-3 fix).
func (db *DB) MarkOAuthPending(id string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	acc, ok := db.Accounts[id]
	if !ok || acc == nil {
		return
	}
	acc.Credentials = nil
	acc.RefreshToken = ""
	acc.TokenExpires = time.Time{}
	acc.UserInfo = nil
	acc.Status = "oauth_pending"
	acc.UpdatedAt = time.Now()
	acc.RecomputeCallGate(acc.UpdatedAt)
	db.dirty = true
	db.txtDirty = true
}

// SnapshotAccount returns a deep copy of the account with the given ID.
// The caller can freely read/mutate the returned copy without racing
// against chat goroutines that modify the live pointer (BUG-6 fix).
func (db *DB) SnapshotAccount(id string) (*Account, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	a, ok := db.Accounts[id]
	if !ok {
		return nil, errors.New("account not found")
	}
	return deepCopyAccount(a), nil
}

// SnapshotList returns deep copies of all accounts. Safe for concurrent
// read/mutation by GET handlers without racing chat goroutines (BUG-6 fix).
func (db *DB) SnapshotList() []*Account {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]*Account, 0, len(db.Accounts))
	for _, a := range db.Accounts {
		out = append(out, deepCopyAccount(a))
	}
	return out
}

// deepCopyAccount produces an isolated copy. Scalar fields + pointer structs
// are copied; Metadata/UserInfo/Models MUST NOT be shared with the live
// record — cnwatch / cnchat / panel writers mutate them outside db.mu while
// saveLocked marshals them inside the lock (concurrent map read/write =
// process fatal).
func deepCopyAccount(a *Account) *Account {
	if a == nil {
		return nil
	}
	cp := *a // shallow copy — all scalar fields + pointer fields copied
	if a.Quota != nil {
		q := *a.Quota
		cp.Quota = &q
	}
	if a.Credentials != nil {
		c := *a.Credentials
		if a.Credentials.DpopKeyPair != nil {
			dp := *a.Credentials.DpopKeyPair
			c.DpopKeyPair = &dp
		}
		cp.Credentials = &c
	}
	if a.Metadata != nil {
		md := make(map[string]string, len(a.Metadata))
		for k, v := range a.Metadata {
			md[k] = v
		}
		cp.Metadata = md
	}
	if a.UserInfo != nil {
		ui := make(map[string]any, len(a.UserInfo))
		for k, v := range a.UserInfo {
			ui[k] = v
		}
		cp.UserInfo = ui
	}
	if a.Models != nil {
		mdl := make([]string, len(a.Models))
		copy(mdl, a.Models)
		cp.Models = mdl
	}
	return &cp
}

func openedLabel(opened bool) string {
	if opened {
		return "是"
	}
	return "否"
}

// formatCredentialLine intentionally omits passwords and AK/SK. accounts.txt is
// an operational inventory, not a credential export (BUG-4 fix).
func formatCredentialLine(a *Account) string {
	if a == nil {
		return ""
	}
	email := strings.TrimSpace(a.Email)
	if email == "" {
		return ""
	}
	return email + "----开通:" + openedLabel(a.Opened)
}

func (db *DB) rewriteAccountsTxtLocked() error {
	var b strings.Builder
	for _, a := range db.Accounts {
		line := formatCredentialLine(a)
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(db.AccountsTxtPath(), []byte(b.String()), 0o600)
}

// RefreshLock 按账号 ID 提供单飞互斥。cnwatch（后台）与 cnchat（在线
// inline 续期）两条刷新通道争抢同一账号时，STS 服务端对 refresh token
// 做一次性轮换检测：先用旧 token 的一方成功，后到的一方拿到
// "has been used"（当前分类为 RefreshTokenExpiredError 终态），会被
// 误判成"凭证彻底失效"而永久摘除账号。持锁进入后先重读活账号，
// refresh token 已被轮换则放弃本次刷新（已是新 token）。
func (db *DB) RefreshLock(accountID string) func() {
	db.mu.Lock()
	lk, ok := db.refreshLocks[accountID]
	if !ok {
		lk = &sync.Mutex{}
		if db.refreshLocks == nil {
			db.refreshLocks = map[string]*sync.Mutex{}
		}
		db.refreshLocks[accountID] = lk
	}
	db.mu.Unlock()
	lk.Lock()
	return lk.Unlock
}

// Mutate 在持锁状态下对活账号应用 fn（fn 不得再发起 IO，也不得
// 再回调 db 其他方法）。解决「Get→锁外改字段→Upsert」的多步竞态：
// Get/Upsert 本身带锁，但中间窗口里别的写者可能已刷过同一账号。
// fn 返回非 nil error 时写回被撤销，且不会触发 markDirty。
func (db *DB) Mutate(id string, fn func(*Account) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	a, ok := db.Accounts[id]
	if !ok {
		return errors.New("account not found")
	}
	if fn == nil {
		return nil
	}
	// 深拷贝回滚：浅拷贝只保住标量，fn 里改过的 Metadata/Credentials/Quota
	// 会留在库内。恢复时也原地写回，不换指针身份——外部可能仍持有它。
	bak := deepCopyAccount(a)
	if err := fn(a); err != nil {
		if bak != nil {
			*a = *bak
		}
		return err
	}
	a.UpdatedAt = time.Now()
	db.markDirtyLocked()
	return nil
}

// markDirtyLocked 必须在持锁状态下调用（由 Mutate 使用）。
func (db *DB) markDirtyLocked() { db.dirty = true }

func (db *DB) Delete(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.Accounts[id]; !ok {
		return errors.New("account not found")
	}
	delete(db.Accounts, id)
	// Immediate flush on delete — don't rely on debounced writer.
	// If process crashes before the 2s debounce, deleted accounts would reappear.
	if err := db.saveLocked(); err != nil {
		return err
	}
	return db.rewriteAccountsTxtLocked()
}

// Save flushes dirty state and stops the debounced writer. Called on shutdown.
func (db *DB) Save() error {
	// Stop the debounced writer and wait for final flush
	select {
	case <-db.writeStop:
		// already closed
	default:
		close(db.writeStop)
		<-db.writeDone
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.saveLocked()
}

// AccountsTxtPath is the plain credential dump next to accounts.json.
func (db *DB) AccountsTxtPath() string {
	return filepath.Join(filepath.Dir(db.path), "accounts.txt")
}

// AppendCredentialLine writes/updates one non-sensitive account line into accounts.txt.
func (db *DB) AppendCredentialLine(a *Account) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.rewriteAccountsTxtLocked()
}

// RewriteAccountsTxt rebuilds accounts.txt without secrets.
func (db *DB) RewriteAccountsTxt() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.rewriteAccountsTxtLocked()
}
