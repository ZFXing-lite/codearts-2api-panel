// Package cnwatch 是「国内码道」账号的后台 watchdog：
// 周期性执行 token 自动刷新（含重新登录打标）、每日签到 / 福利自动领取、
// 保活心跳。对齐 codearts2api-cn internal/pool 的独立实现。
//
// 所有上游故障都不致命：只记日志 / 写 Metadata，绝不 panic 或退出进程。
package cnwatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"go-codearts/internal/cnup"
	"go-codearts/internal/config"
	"go-codearts/internal/kit"
	"go-codearts/internal/store"
)

// Metadata 键（账号 Account.Metadata）。
const (
	mdClientID       = "client_id"               // OAuth client_id（兼容旧导入）
	mdClientIDLegacy = "cn_client_id"            // store/cnimport 写入的键
	mdCodeVerifier   = "cn_code_verifier"        // PKCE verifier（刷新时必传）
	mdDPoPKey        = "cn_dpop_private_key"     // DPoPPrivateJWK 的 JSON 序列化
	mdNeedsRelogin   = "cn_needs_relogin"        // refresh_token 终态失效标记
	mdLastActive     = "cn_last_active"          // 最近一次 chat 活动时间（RFC3339）
	mdLastKeepalive  = "cn_last_keepalive"       // 最近一次成功/尝试心跳时间
	mdCheckinDate    = "cn_last_checkin_date"    // 最近签到成功的上海日历日（2026-02-14）
	mdCheckinMsg     = "cn_last_checkin_msg"     // 最近签到结果摘要（面板展示）
	mdCheckinCredits = "cn_last_checkin_credits" // 最近签到获得积分
	mdCheckinAt      = "cn_last_checkin_at"      // 最近签到时间戳（RFC3339）
)

// cnDay 以 Asia/Shanghai 日历日为准（签到日界与上游一致）。
var cnDay = time.FixedZone("CST", 8*3600)

func cnDateKey(t time.Time) string { return t.In(cnDay).Format("2006-01-02") }

// mdCheckinDate 等键同时暴露给 api 包（面板读取）。
const (
	MetaCheckinDate  = mdCheckinDate
	MetaCheckinMsg   = mdCheckinMsg
	MetaCheckinAt    = mdCheckinAt
	MetaLastActive   = mdLastActive
	MetaLastPing     = mdLastKeepalive
	MetaNeedsRelogin = mdNeedsRelogin
)

// Client 抽象 cnup.Client，便于测试注入假实现；*cnup.Client 天然满足。
type Client interface {
	RefreshToken(ctx context.Context, cfg cnup.LoginConfig, refreshToken, codeVerifier string, dpop cnup.DPoPPrivateJWK) (*cnup.TokenResponse, error)
	Checkin(ctx context.Context, cred cnup.SignCredential) (cnup.CheckinResult, error)
	ClaimBenefit(cred cnup.SignCredential) error
	ChatStream(ctx context.Context, chatID string, messages []cnup.ChatMessage, traceID string, cred cnup.SignCredential, userName string, model string, benefit bool) (io.ReadCloser, error)
	FetchModels(acct *cnup.Account) ([]cnup.ModelInfo, error)
}

// Watcher CN 账号 watchdog。
type Watcher struct {
	db      *store.DB
	cfg     config.Config
	client  Client
	now     func() time.Time // 测试可替换时钟
	running atomic.Bool      // Run 防重入；意外二次调用直接退出
}

// New 构造 watchdog，内部自建 cnup 客户端。
func New(db *store.DB, cfg config.Config) *Watcher {
	return NewWithClient(db, cfg, cnup.New(cfg.CNUpstreamTimeout))
}

// NewWithClient 注入自定义客户端（测试用）。
func NewWithClient(db *store.DB, cfg config.Config, client Client) *Watcher {
	if cfg.CNWatchPoll <= 0 {
		cfg.CNWatchPoll = 30 * time.Minute
	}
	if cfg.CNRefreshSkew <= 0 {
		cfg.CNRefreshSkew = 30 * time.Minute
	}
	if cfg.CNKeepaliveWindow <= 0 {
		cfg.CNKeepaliveWindow = 10 * time.Minute
	}
	if cfg.CNKeepaliveInterval <= 0 {
		cfg.CNKeepaliveInterval = 15 * time.Minute
	}
	return &Watcher{db: db, cfg: cfg, client: client, now: time.Now}
}

// Run 启动循环：立即扫描一次，随后每 CNWatchPoll 一次，直到 ctx 取消。
// 不可重入：上层热重载误调两次会叠加双刷竞态。
func (w *Watcher) Run(ctx context.Context) {
	if !w.running.CompareAndSwap(false, true) {
		log.Printf("cnwatch: Run called while already running, ignoring")
		return
	}
	defer w.running.Store(false)
	log.Printf("cnwatch: started poll=%s refresh_skew=%s keepalive_window=%s keepalive_interval=%s checkin=%v",
		w.cfg.CNWatchPoll, w.cfg.CNRefreshSkew, w.cfg.CNKeepaliveWindow, w.cfg.CNKeepaliveInterval, w.cfg.CNCreditsAutoClaim)
	w.scanOnce(ctx)
	t := time.NewTicker(w.cfg.CNWatchPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("cnwatch: stopped: %v", ctx.Err())
			return
		case <-t.C:
			w.scanOnce(ctx)
		}
	}
}

// scanOnce 对全部 Channel=="cn" 账号串行执行一轮 watch。
func (w *Watcher) scanOnce(ctx context.Context) {
	live := map[string]bool{}
	for _, a := range w.db.List() {
		if a == nil || store.NormalizeChannel(a.Channel) != store.ChannelCN {
			continue
		}
		if a.UserID != "" {
			live[a.UserID] = true
		}
		if ctx.Err() != nil {
			return
		}
		w.watchAccount(ctx, a.ID)
	}
	// 删号/换号后，旧账号的模型目录会一直驻留（known 索引只增不减）。
	if n := cnup.PruneCatalogs(live); n > 0 {
		log.Printf("cnwatch: pruned %d stale model catalogs", n)
	}
	if real, ok := w.client.(*cnup.Client); ok {
		if n := real.PruneSrcFail(live); n > 0 {
			log.Printf("cnwatch: pruned %d stale source-failure entries", n)
		}
	}
}

// watchAccount 单账号一轮：刷新 → 签到 → 福利 → 保活。单步失败不影响后续步骤。
func (w *Watcher) watchAccount(ctx context.Context, id string) {
	acc, err := w.db.SnapshotAccount(id)
	if err != nil {
		return
	}
	if acc.Metadata == nil {
		acc.Metadata = map[string]string{}
	}
	if acc.Metadata[mdNeedsRelogin] == "true" {
		// 已判定需要重新登录，等待操作者处理，不再打上游。
		return
	}

	if w.refreshIfDue(ctx, acc) {
		return // refresh_token 终态失效已打标，本轮不再动该账号
	}

	cred := signCred(acc)
	if cred.SecurityToken == "" {
		log.Printf("cnwatch: %s: no access token, skip checkin/benefit/keepalive", acc.ID)
		return
	}
	if w.cfg.CNCreditsAutoClaim {
		w.checkin(ctx, acc, cred)
	}
	if w.cfg.CNBenefitAutoClaim {
		w.claimBenefit(acc, cred)
	}
	w.refreshCatalog(acc)
	w.keepalive(ctx, acc, cred)
}

// refreshCatalog 在网关进程内做模型目录发现。
//
// 三路发现逻辑（OpenAI 兼容端点 / agent-center / 快照）在 cnup 里都写好了，
// 但唯一调用者是独立的 cmd/cnmodels CLI —— 网关自己从不刷新，于是
// catalog 恒空：/v1/models 的 cn: 模型为空、面板模型页 count=0、
// IsBenefitModel 永远回落三个冷启动种子（新福利模型漏带 maas_type 会 404）。
// 按 CatalogTTL 节流，失败下一轮再试。
func (w *Watcher) refreshCatalog(acc *store.Account) {
	if acc == nil || acc.UserID == "" {
		return
	}
	if !cnup.AccountCatalogStale(acc.UserID) {
		return
	}
	acct := &cnup.Account{
		UserID:         acc.UserID,
		CloudDragonTok: acc.AccessToken,
	}
	if acc.Credentials != nil {
		acct.AccessKeyID = acc.Credentials.AccessKeyID
		acct.SecretAccessKey = acc.Credentials.SecretAccessKey
		if acct.CloudDragonTok == "" {
			acct.CloudDragonTok = acc.Credentials.SecurityToken
		}
	}
	ids, err := w.client.FetchModels(acct)
	if err != nil {
		log.Printf("cnwatch: %s: catalog refresh failed: %v", acc.ID, err)
		return
	}
	log.Printf("cnwatch: %s: catalog refreshed (%d models)", acc.ID, len(ids))
}

// refreshIfDue 距 TokenExpires < CNRefreshSkew 时刷新并写回。
// 返回 true 表示 refresh_token 已终态失效、账号被标记需要重新登录。
func (w *Watcher) refreshIfDue(ctx context.Context, acc *store.Account) bool {
	if acc.RefreshToken == "" {
		return false
	}
	exp := acc.TokenExpires
	if !exp.IsZero() && w.now().Add(w.cfg.CNRefreshSkew).Before(exp) {
		return false // 还很新
	}
	loginCfg := cnup.DefaultLoginConfig()
	if cid := firstNonEmpty(acc.Metadata[mdClientID], acc.Metadata[mdClientIDLegacy]); cid != "" {
		loginCfg.ClientID = cid
	}
	var dpop cnup.DPoPPrivateJWK
	if raw := acc.Metadata[mdDPoPKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &dpop); err != nil {
			log.Printf("cnwatch: %s: bad dpop jwk in metadata: %v", acc.ID, err)
		}
	}
	unlock := w.db.RefreshLock(acc.ID)
	defer unlock()
	// 重读活账号：可能已被 cnchat inline 刷新抢先一步完成
	freshAcc, err := w.db.Get(acc.ID)
	if err == nil && freshAcc.RefreshToken != acc.RefreshToken {
		log.Printf("cnwatch: %s: refresh token rotated by another path, skip", acc.ID)
		return false // 已被另一侧刷新，放弃本次避免双轮换
	}
	res, err := w.client.RefreshToken(ctx, loginCfg, acc.RefreshToken, acc.Metadata[mdCodeVerifier], dpop)
	if err != nil {
		var expired *cnup.RefreshTokenExpiredError
		if errors.As(err, &expired) {
			log.Printf("cnwatch: %s: refresh token expired, needs re-login: %v", acc.ID, err)
			// Mutate 原子写入，避免 acc 已是旧快照时覆盖别处刷新的字段
			if merr := w.db.Mutate(acc.ID, func(fresh *store.Account) error {
				if fresh.Metadata == nil {
					fresh.Metadata = map[string]string{}
				}
				fresh.Metadata[mdNeedsRelogin] = "true"
				fresh.LastError = err.Error()
				return nil
			}); merr != nil {
				log.Printf("cnwatch: %s: persist relogin mark: %v", acc.ID, merr)
			}
			return true
		}
		log.Printf("cnwatch: %s: refresh failed (transient): %v", acc.ID, err)
		return false
	}
	// Mutate 在持锁状态下原子改活账号；快照 acc 的 TokenExpires 后续给日志用
	var newExp time.Time
	if merr := w.db.Mutate(acc.ID, func(fresh *store.Account) error {
		applyTokenResponse(fresh, res)
		newExp = fresh.TokenExpires
		return nil
	}); merr != nil {
		log.Printf("cnwatch: %s: persist refreshed token: %v", acc.ID, merr)
		return false
	}
	log.Printf("cnwatch: %s: token refreshed, expires=%s", acc.ID, newExp.Format(time.RFC3339))
	return false
}

// applyTokenResponse 把刷新结果写回账号字段。
func applyTokenResponse(acc *store.Account, res *cnup.TokenResponse) {
	if res == nil {
		return
	}
	c := res.Credentials
	if c.SecurityToken != "" {
		acc.AccessToken = c.SecurityToken
	}
	if res.RefreshToken != "" {
		acc.RefreshToken = res.RefreshToken
	}
	if exp := store.ParseFlexibleTime(c.Expiration); !exp.IsZero() {
		acc.TokenExpires = exp
	}
	if res.UserID != "" {
		acc.UserID = res.UserID
	}
	if c.AccessKeyID != "" && c.SecretAccessKey != "" {
		acc.Credentials = &store.Credentials{
			AccessKeyID:     c.AccessKeyID,
			SecretAccessKey: c.SecretAccessKey,
			SecurityToken:   c.SecurityToken,
			Expiration:      acc.TokenExpires,
			AuthMode:        "oauth_sts",
		}
	} else if acc.Credentials != nil {
		// STS 的 AK/SK 与 security_token 是成对的。上游只轮换 token、不重发
		// AK/SK 时，必须把签名用的 security_token 一并更新，否则账号会被
		// 调度器反复选中、反复 401。
		acc.Credentials.SecurityToken = acc.AccessToken
		acc.Credentials.Expiration = acc.TokenExpires
	}
}

// checkin 每日签到 / 新手福利领取。结果落库到账号 Metadata，
// 面板据此展示"今日已签"状态；本地闸门保证每账号每日只打一次上游。
func (w *Watcher) checkin(ctx context.Context, acc *store.Account, cred cnup.SignCredential) {
	today := cnDateKey(w.now())
	if acc.Metadata[mdCheckinDate] == today {
		return // 今日已签，本地闸门拦住重复请求
	}
	res, err := w.client.Checkin(ctx, cred)
	msg := ""
	credits := res.Credits
	if err != nil {
		msg = err.Error()
		log.Printf("cnwatch: %s: checkin failed: %v", acc.ID, err)
	} else if res.Message != "" || res.Credits > 0 {
		msg = res.Message
		log.Printf("cnwatch: %s: checkin ok=%v credits=%g msg=%s", acc.ID, res.OK, res.Credits, res.Message)
	}
	// 落库：失败也记录 msg（不写日期，下一轮重试），成功写日期关闭今日闸门。
	_ = w.db.Mutate(acc.ID, func(fresh *store.Account) error {
		if fresh.Metadata == nil {
			fresh.Metadata = map[string]string{}
		}
		fresh.Metadata[mdCheckinAt] = w.now().Format(time.RFC3339)
		fresh.Metadata[mdCheckinMsg] = truncate(msg, 200)
		// 只有真正拿到活动才算"今日已签"。上游 200 但活动列表为空
		// （灰度/配置延迟/部分故障）不等于签过，否则当天漏签且无法补。
		if err == nil && res.Matched > 0 {
			fresh.Metadata[mdCheckinDate] = today
			fresh.Metadata[mdCheckinCredits] = strconv.FormatFloat(credits, 'f', -1, 64)
		}
		return nil
	})
}

// truncate 截断过长文案（Metadata 只存摘要）。按字符边界切，
// 否则中文错误信息写进 accounts.json 就是半个字符的乱码。
func truncate(s string, n int) string {
	return kit.TruncateUTF8(s, n)
}

// claimBenefit 限时福利领取（cnup 内置幂等便利方法）。
func (w *Watcher) claimBenefit(acc *store.Account, cred cnup.SignCredential) {
	if err := w.client.ClaimBenefit(cred); err != nil {
		log.Printf("cnwatch: %s: benefit claim failed: %v", acc.ID, err)
	}
}

// keepalive 账号沉寂超过 CNKeepaliveWindow 且距上次心跳超过
// CNKeepaliveInterval 时发极短 ping。失败不致命。
func (w *Watcher) keepalive(ctx context.Context, acc *store.Account, cred cnup.SignCredential) {
	now := w.now()
	lastActive := parseTime(acc.Metadata[mdLastActive])
	if !lastActive.IsZero() && now.Sub(lastActive) <= w.cfg.CNKeepaliveWindow {
		return // 近期有活跃，无需心跳
	}
	lastPing := parseTime(acc.Metadata[mdLastKeepalive])
	if !lastPing.IsZero() && now.Sub(lastPing) <= w.cfg.CNKeepaliveInterval {
		return // 心跳间隔未到
	}
	chatID := acc.ChatID
	if chatID == "" {
		chatID = "watch-" + randHex(8)
	}
	model := w.cfg.CNDefaultModel
	if model == "" {
		model = "snap-chat"
	}
	rc, err := w.client.ChatStream(ctx, chatID,
		[]cnup.ChatMessage{{Role: "user", Content: "ping"}},
		"cnwatch-"+randHex(4), cred, acc.Email, model, false)
	if err != nil {
		log.Printf("cnwatch: %s: keepalive ping failed: %v", acc.ID, err)
		// 失败也更新时间戳，避免连续失败造成请求风暴。
	} else {
		// 读掉头部即关闭，不消费整条流。
		buf := make([]byte, 512)
		_, _ = rc.Read(buf)
		_ = rc.Close()
		log.Printf("cnwatch: %s: keepalive ping sent (chat=%s)", acc.ID, chatID)
	}
	// 只能 Mutate 单个字段：Upsert 是整对象替换，会把并发期间 cnchat 刚刷新
	// 出来的 AccessToken/RefreshToken 写回旧值。refresh token 一次性轮换，
	// 旧值回写后下一次刷新会被 STS 判"已使用"，账号直接永久失效。
	if uerr := w.db.Mutate(acc.ID, func(fresh *store.Account) error {
		if fresh.Metadata == nil {
			fresh.Metadata = map[string]string{}
		}
		fresh.Metadata[mdLastKeepalive] = now.Format(time.RFC3339)
		return nil
	}); uerr != nil {
		log.Printf("cnwatch: %s: persist keepalive ts: %v", acc.ID, uerr)
	}
}

func signCred(acc *store.Account) cnup.SignCredential {
	cred := cnup.SignCredential{SecurityToken: acc.AccessToken}
	if acc.Credentials != nil {
		cred.AccessKeyID = acc.Credentials.AccessKeyID
		cred.SecretAccessKey = acc.Credentials.SecretAccessKey
		if cred.SecurityToken == "" {
			cred.SecurityToken = acc.Credentials.SecurityToken
		}
	}
	return cred
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
