package api

// 国内码道（CN 渠道）面板 API。
// 面板「国内码道」页的全部数据与操作都从本文件出去：
//   GET  /api/cn/dashboard              渠道状态总览 + 账号明细 + 配置快照 + 模型目录
//   POST /api/cn/import                 从 CN_AUTH_DIR（auths/*.json）一键导入
//   POST /api/cn/accounts/{id}/refresh  手动刷新 token
//   POST /api/cn/accounts/{id}/checkin  手动签到 / 领积分
//   POST /api/cn/accounts/{id}/relogin-clear  清 cn_needs_relogin 标记（重登后兜底）
//   GET  /api/cn/models                 CN 模型目录（裸名 + cn: 前缀）

import (
	"context"
	"encoding/json"
	"errors"
	"go-codearts/internal/kit"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go-codearts/internal/cnauth"
	"go-codearts/internal/cnup"
	"go-codearts/internal/cnwatch"
	"go-codearts/internal/store"
)

// cnAccountView 单账号面板视图。
type cnAccountView struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	UserID        string `json:"user_id"`
	ChatID        string `json:"chat_id"`
	Status        string `json:"status"`
	CallGate      string `json:"call_gate"`
	TokenExpires  string `json:"token_expires"`
	TokenState    string `json:"token_state"` // ok | due | expired | none
	HasAKSK       bool   `json:"has_aksk"`
	NeedsRelogin  bool   `json:"needs_relogin"`
	LastError     string `json:"last_error"`
	LastActive    string `json:"last_active"`
	LastKeepalive string `json:"last_keepalive"`
	CheckinDate   string `json:"checkin_date"`
	CheckinMsg    string `json:"checkin_msg"`
	CheckinAt     string `json:"checkin_at"`
	ModelCount    int    `json:"model_count"`
	SuccessCount  int64  `json:"success_count"`
	FailCount     int64  `json:"fail_count"`
	Disabled      bool   `json:"disabled"`

	// 积分余额（需要 CN_BILLING_* 配置；未配置时 CreditKnown=false）
	CreditRemain  int64           `json:"credit_remain"`
	CreditKnown   bool            `json:"credit_known"`
	CreditFetched string          `json:"credit_fetched,omitempty"`
	CreditError   string          `json:"credit_error,omitempty"`
	CreditPacks   json.RawMessage `json:"credit_packages,omitempty"`
	// 最近一次签到领到的积分（本地记录，与余额不是同一口径）
	LastCheckinCredits string `json:"last_checkin_credits,omitempty"`
}

// cnCreditHint 说明池子总积分的口径。
// 未知余额的账号不进合计——必须讲清楚，否则"池子总积分"看着完整其实缺人。
func cnCreditHint(configured bool) string {
	if !configured {
		return "未启用余额查询：需配置 CN_BILLING_BASE_URL 与 CN_BILLING_PRODUCT_CODE" +
			"（华为 CodeArts 没有公开固定的余额端点，产品码取错会查到别的产品额度，所以不猜）"
	}
	return "合计只统计已查到余额的账号；查询失败的账号不计入，原因见各账号的 credit_error"
}

// handleCNDashboard 汇总 CN 渠道运行状态，供面板「国内码道」页一次性渲染。
func (s *Server) handleCNDashboard(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	accounts := s.db.SnapshotList()
	skew := s.cfg.CNRefreshSkew
	if skew <= 0 {
		skew = 30 * time.Minute
	}

	views := make([]cnAccountView, 0, 8)
	callable, needsRelogin, tokenDue, checkinToday, disabledN := 0, 0, 0, 0, 0
	var poolCredits int64          // 已知余额的账号合计
	creditKnown, creditUnk := 0, 0 // 已知 / 未知余额的账号数
	creditConfigured := strings.TrimSpace(s.cfg.CNBillingBaseURL) != "" &&
		strings.TrimSpace(s.cfg.CNBillingProductCode) != ""
	var lastActive time.Time
	modelSet := map[string]int{} // model -> 可见账号数
	today := cnTodayKey(now)

	for _, a := range accounts {
		if a == nil || store.NormalizeChannel(a.Channel) != store.ChannelCN {
			continue
		}
		a.RecomputeCallGate(now)
		v := cnAccountView{
			ID:            a.ID,
			Email:         a.Email,
			UserID:        a.UserID,
			ChatID:        a.ChatID,
			Status:        a.Status,
			CallGate:      a.CallGate,
			HasAKSK:       a.Credentials != nil && a.Credentials.AccessKeyID != "",
			NeedsRelogin:  a.Metadata[cnwatch.MetaNeedsRelogin] == "true",
			LastError:     a.LastError,
			LastActive:    a.Metadata[cnwatch.MetaLastActive],
			LastKeepalive: a.Metadata[cnwatch.MetaLastPing],
			CheckinDate:   a.Metadata[cnwatch.MetaCheckinDate],
			CheckinMsg:    a.Metadata[cnwatch.MetaCheckinMsg],
			CheckinAt:     a.Metadata[cnwatch.MetaCheckinAt],
			SuccessCount:  a.SuccessCount,
			FailCount:     a.FailCount,
			Disabled:      a.Disabled,
		}
		if raw := strings.TrimSpace(a.Metadata[cnwatch.MetaCreditRemain]); raw != "" {
			if n, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
				v.CreditRemain, v.CreditKnown = n, true
			}
		}
		v.CreditFetched = a.Metadata[cnwatch.MetaCreditFetched]
		v.CreditError = a.Metadata[cnwatch.MetaCreditErr]
		if pk := strings.TrimSpace(a.Metadata[cnwatch.MetaCreditPackages]); pk != "" {
			v.CreditPacks = json.RawMessage(pk)
		}
		v.LastCheckinCredits = a.Metadata["cn_last_checkin_credits"]
		// token 状态判读
		switch {
		case a.TokenExpires.IsZero():
			v.TokenState = "none"
		case a.TokenExpires.Before(now):
			v.TokenState = "expired"
			tokenDue++
		case a.TokenExpires.Sub(now) < skew:
			v.TokenState = "due"
			tokenDue++
		default:
			v.TokenState = "ok"
		}
		if !a.TokenExpires.IsZero() {
			v.TokenExpires = a.TokenExpires.Format(time.RFC3339)
		}
		if v.CreditKnown {
			poolCredits += v.CreditRemain
			creditKnown++
		} else {
			creditUnk++
		}
		if v.NeedsRelogin {
			needsRelogin++
		}
		if a.Disabled {
			disabledN++
		}
		if (a.CallGate == store.CallGateReady || a.CallGate == store.CallGateQuotaExhausted) &&
			!a.Disabled && !v.NeedsRelogin {
			callable++
		}
		if v.CheckinDate == today {
			checkinToday++
		}
		if t := cnParseTime(v.LastActive); t.After(lastActive) {
			lastActive = t
		}
		if infos, ok := cnup.AccountModels(a.UserID); ok {
			v.ModelCount = len(infos)
			for _, m := range infos {
				if m.ID != "" {
					modelSet[m.ID]++
				}
			}
		} else if len(a.Models) > 0 {
			// 目录还没发现（冷启动 / 拉取失败）时，账号自身记录的模型名
			// 也要显示出来，否则面板一片空白，看着像国内渠道没接上。
			v.ModelCount = len(a.Models)
			for _, m := range a.Models {
				if m = strings.TrimSpace(m); m != "" {
					modelSet[m]++
				}
			}
		}
		views = append(views, v)
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Email < views[j].Email })

	type modelView struct {
		ID       string `json:"id"`
		Prefixed string `json:"prefixed"`
		Benefit  bool   `json:"benefit"`
		Accounts int    `json:"accounts"`
	}
	models := make([]modelView, 0, len(modelSet))
	for id, n := range modelSet {
		benefit := false
		for _, v := range views {
			if cnup.IsBenefitModel(v.ID, id) {
				benefit = true
				break
			}
		}
		models = append(models, modelView{ID: id, Prefixed: "cn:" + id, Benefit: benefit, Accounts: n})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })

	authDir := s.cfg.CNAuthDir
	authDirOK := false
	authFileCount := 0
	if authDir != "" {
		if entries, err := os.ReadDir(authDir); err == nil {
			authDirOK = true
			for _, e := range entries {
				if !e.IsDir() && cnIsAuthFile(e.Name()) {
					authFileCount++
				}
			}
		}
	}

	cfgView := []map[string]any{
		{"key": "CN_ENABLED", "label": "渠道总开关", "value": cnBoolZh(s.cfg.CNEnabled)},
		{"key": "CN_AUTH_DIR", "label": "凭证目录", "value": authDir},
		{"key": "CN_DEFAULT_MODEL", "label": "保活/默认模型", "value": s.cfg.CNDefaultModel},
		{"key": "CN_WATCH_POLL", "label": "看护轮询周期", "value": s.cfg.CNWatchPoll.String()},
		{"key": "CN_REFRESH_SKEW", "label": "token 提前续期", "value": s.cfg.CNRefreshSkew.String()},
		{"key": "CN_KEEPALIVE_WINDOW", "label": "保活沉寂窗口", "value": s.cfg.CNKeepaliveWindow.String()},
		{"key": "CN_KEEPALIVE_INTERVAL", "label": "保活心跳间隔", "value": s.cfg.CNKeepaliveInterval.String()},
		{"key": "CN_MAX_CONCURRENT", "label": "单账号并发上限", "value": strconv.Itoa(s.cfg.CNMaxConcurrent)},
		{"key": "CN_UPSTREAM_TIMEOUT", "label": "上游超时", "value": s.cfg.CNUpstreamTimeout.String()},
		{"key": "CN_ERR_THRESHOLD", "label": "连败熔断阈值", "value": strconv.Itoa(s.cfg.CNErrThreshold)},
		{"key": "CN_ERR_COOLDOWN", "label": "熔断冷却", "value": s.cfg.CNErrCooldown.String()},
		{"key": "CN_SOFT_COOLDOWN", "label": "软冷却", "value": s.cfg.CNSoftCooldown.String()},
		{"key": "CN_CREDITS_AUTO_CLAIM", "label": "每日签到自动领", "value": cnBoolZh(s.cfg.CNCreditsAutoClaim)},
		{"key": "CN_BENEFIT_AUTO_CLAIM", "label": "福利模型自动领", "value": cnBoolZh(s.cfg.CNBenefitAutoClaim)},
	}

	lastActiveStr := ""
	if !lastActive.IsZero() {
		lastActiveStr = lastActive.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":         s.cfg.CNEnabled,
		"accounts":        len(views),
		"callable":        callable,
		"needs_relogin":   needsRelogin,
		"token_due":       tokenDue,
		"disabled":        disabledN,
		"checkin_today":   checkinToday,
		"today":           today,
		"last_active":     lastActiveStr,
		"auth_dir":        authDir,
		"auth_dir_ok":     authDirOK,
		"auth_file_count": authFileCount,
		"account_views":   views,
		"models":          models,
		"config":          cfgView,
		"credits": map[string]any{
			"pool_total": poolCredits,
			"known":      creditKnown,
			"unknown":    creditUnk,
			"configured": creditConfigured,
			"hint":       cnCreditHint(creditConfigured),
		},
	})
}

// handleCNImport 从 CN_AUTH_DIR 导入 auths/*.json（cnlogin 产出的凭证）。
func (s *Server) handleCNImport(w http.ResponseWriter, r *http.Request) {
	dir := s.cfg.CNAuthDir
	if dir == "" {
		writeErr(w, http.StatusBadRequest, "CN_AUTH_DIR 未配置")
		return
	}
	n, err := s.db.ImportCNAuthDir(dir)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "导入失败: "+err.Error())
		return
	}
	s.SyncShardFromDB()
	writeJSON(w, http.StatusOK, map[string]any{"imported": n, "dir": dir})
}

// cnLoadAccount 取一个 CN 账号快照，非 CN 或不存在则返回 404。
func (s *Server) cnLoadAccount(w http.ResponseWriter, id string) *store.Account {
	a, err := s.db.SnapshotAccount(id)
	if err != nil || a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return nil
	}
	if store.NormalizeChannel(a.Channel) != store.ChannelCN {
		writeErr(w, http.StatusBadRequest, "不是国内码道账号")
		return nil
	}
	return a
}

// handleCNRefresh 手动刷新 token（与 cnwatch / chat inline 共用 RefreshLock）。
func (s *Server) handleCNRefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acc := s.cnLoadAccount(w, id)
	if acc == nil {
		return
	}
	if acc.RefreshToken == "" {
		writeErr(w, http.StatusBadRequest, "该账号没有 refresh_token，请用 cnlogin 重新登录")
		return
	}
	if err := s.cnRefresh(acc); err != nil {
		if s.cnMarkReloginErr(acc, err) {
			writeErr(w, http.StatusBadGateway, "refresh_token 已失效，请用 cnlogin 重新登录")
			return
		}
		writeErr(w, http.StatusBadGateway, "刷新失败: "+err.Error())
		return
	}
	fresh, _ := s.db.SnapshotAccount(id)
	resp := map[string]any{"ok": true}
	if fresh != nil {
		resp["token_expires"] = fresh.TokenExpires.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCNCheckin 手动触发签到（面板按钮，绕过每日闸门语义：手动即强制）。
func (s *Server) handleCNCheckin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acc := s.cnLoadAccount(w, id)
	if acc == nil {
		return
	}
	if acc.AccessToken == "" {
		writeErr(w, http.StatusBadRequest, "账号没有 access token")
		return
	}
	cred := cnup.SignCredential{SecurityToken: acc.AccessToken}
	if acc.Credentials != nil {
		cred.AccessKeyID = acc.Credentials.AccessKeyID
		cred.SecretAccessKey = acc.Credentials.SecretAccessKey
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.CNUpstreamTimeout)
	defer cancel()
	res, err := s.cnUpstream().Checkin(ctx, cred)
	now := time.Now()
	today := cnTodayKey(now)
	msg := res.Message
	if err != nil {
		msg = err.Error()
	}
	_ = s.db.Mutate(id, func(fresh *store.Account) error {
		if fresh.Metadata == nil {
			fresh.Metadata = map[string]string{}
		}
		fresh.Metadata[cnwatch.MetaCheckinAt] = now.Format(time.RFC3339)
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		fresh.Metadata[cnwatch.MetaCheckinMsg] = msg
		if err == nil {
			fresh.Metadata[cnwatch.MetaCheckinDate] = today
			fresh.Metadata["cn_last_checkin_credits"] = strconv.FormatFloat(res.Credits, 'f', -1, 64)
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "签到失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": res.OK, "credits": res.Credits, "message": res.Message, "campaigns": res.Campaigns,
	})
}

// handleCNReloginClear 手动摘掉 needs_relogin 标记（重登后的兜底路径；
// 正常路径由 cnlogin 导入时自动清除）。
func (s *Server) handleCNReloginClear(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.cnLoadAccount(w, id) == nil {
		return
	}
	_ = s.db.Mutate(id, func(fresh *store.Account) error {
		delete(fresh.Metadata, cnwatch.MetaNeedsRelogin)
		fresh.LastError = ""
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleCNModels CN 模型目录（聚合所有 CN 账号已检出的模型）。
func (s *Server) handleCNModels(w http.ResponseWriter, r *http.Request) {
	accounts := s.db.SnapshotList()
	seen := map[string]int{}
	for _, a := range accounts {
		if a == nil || store.NormalizeChannel(a.Channel) != store.ChannelCN {
			continue
		}
		if infos, ok := cnup.AccountModels(a.UserID); ok {
			for _, m := range infos {
				if m.ID != "" {
					seen[m.ID]++
				}
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "prefixed": "cn:" + id, "accounts": seen[id]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "count": len(out)})
}

func cnTodayKey(t time.Time) string { return t.In(cnFixedZone).Format("2006-01-02") }

var cnFixedZone = time.FixedZone("CST", 8*3600)

func cnParseTime(s string) time.Time { return store.ParseFlexibleTime(s) }

func cnBoolZh(b bool) string {
	if b {
		return "已启用"
	}
	return "已关闭"
}

// cnIsAuthFile 复用 cnauth 判定，避免与 CLI 的命名规则漂移。
func cnIsAuthFile(name string) bool { return cnauth.IsAuthFile(name) }

// cnBillingConfig 组装余额查询配置。
func (s *Server) cnBillingConfig() cnup.BillingConfig {
	return cnup.BillingConfig{
		BaseURL:     s.cfg.CNBillingBaseURL,
		ProductCode: s.cfg.CNBillingProductCode,
		MeterPath:   s.cfg.CNBillingMeterPath,
	}
}

// refreshCNCredit 查一个账号的积分余额并写回 Metadata。
// 返回余额与是否已知（未配置时为 false，不视为错误）。
func (s *Server) refreshCNCredit(ctx context.Context, acc *store.Account) (int64, bool, error) {
	bcfg := s.cnBillingConfig()
	if !bcfg.Configured() {
		return 0, false, cnup.ErrBillingNotConfigured
	}
	cred := cnup.SignCredential{}
	if acc.Credentials != nil {
		cred.AccessKeyID = acc.Credentials.AccessKeyID
		cred.SecretAccessKey = acc.Credentials.SecretAccessKey
		cred.SecurityToken = acc.Credentials.SecurityToken
	}
	if cred.SecurityToken == "" {
		cred.SecurityToken = acc.AccessToken
	}
	if cred.SecurityToken == "" && cred.AccessKeyID == "" {
		return 0, false, errors.New("账号没有可用凭据（access_token / AK-SK 都为空）")
	}
	bal, err := s.cnUpstream().FetchCreditBalance(ctx, bcfg, cred)
	if err != nil {
		_ = s.db.Mutate(acc.ID, func(fresh *store.Account) error {
			if fresh.Metadata == nil {
				fresh.Metadata = map[string]string{}
			}
			fresh.Metadata[cnwatch.MetaCreditErr] = kit.Snippet(err.Error(), 200)
			return nil
		})
		return 0, false, err
	}
	var pk []byte
	if len(bal.Packages) > 0 {
		pk, _ = json.Marshal(bal.Packages)
	}
	remain := bal.Remain
	at := bal.FetchedAt.Format(time.RFC3339)
	_ = s.db.Mutate(acc.ID, func(fresh *store.Account) error {
		if fresh.Metadata == nil {
			fresh.Metadata = map[string]string{}
		}
		fresh.Metadata[cnwatch.MetaCreditRemain] = strconv.FormatInt(remain, 10)
		fresh.Metadata[cnwatch.MetaCreditFetched] = at
		fresh.Metadata[cnwatch.MetaCreditPackages] = string(pk)
		delete(fresh.Metadata, cnwatch.MetaCreditErr)
		return nil
	})
	return remain, true, nil
}

// handleCNCredit 手动查询单个账号的积分余额。
func (s *Server) handleCNCredit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acc := s.cnLoadAccount(w, id)
	if acc == nil {
		return
	}
	if !s.cnBillingConfig().Configured() {
		writeErr(w, http.StatusBadRequest,
			"余额查询未启用：需要配置 CN_BILLING_BASE_URL 与 CN_BILLING_PRODUCT_CODE"+
				"（华为 CodeArts 没有公开固定的余额端点，产品码取错会查到别的产品额度，所以不猜）")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	remain, _, err := s.refreshCNCredit(ctx, acc)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "查询余额失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credit_remain": remain})
}

// handleCNCreditRefreshAll 手动刷新整个 CN 账号池的余额。
// 并发 4：单账号查询很轻，但上游量级不明，别一把梭。
func (s *Server) handleCNCreditRefreshAll(w http.ResponseWriter, r *http.Request) {
	if !s.cnBillingConfig().Configured() {
		writeErr(w, http.StatusBadRequest,
			"余额查询未启用：需要配置 CN_BILLING_BASE_URL 与 CN_BILLING_PRODUCT_CODE")
		return
	}
	list := s.db.List()
	var targets []*store.Account
	for _, a := range list {
		if a != nil && store.IsCNAccount(a) {
			targets = append(targets, a)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	var okN, failN int
	var total int64
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, a := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *store.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			remain, known, err := s.refreshCNCredit(ctx, acc)
			mu.Lock()
			defer mu.Unlock()
			if err != nil || !known {
				failN++
				return
			}
			okN++
			total += remain
		}(a)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": len(targets), "known": okN, "failed": failN, "pool_total": total,
	})
}
