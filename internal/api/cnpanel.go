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
	"net/http"
	"os"
	"sort"
	"strconv"
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
