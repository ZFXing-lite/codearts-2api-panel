package api

// 国内码道面板内置登录。
//
// 华为云 CodeArts 的 OAuth 回调目标是 http://127.0.0.1:{port}/oauth/callback，
// 这个地址由 portal 按授权链接里的 port 参数拼出来，无法改成域名。于是三类
// 部署各有各的路径：
//
//  1. 浏览器与服务同机（含本机 docker + 端口映射）：portal 直接回调本服务
//     的 /oauth/callback，最顺，一次点完。
//  2. 浏览器在别的机器：回调打到那台机器的 127.0.0.1，必然连不上。用户在
//     浏览器地址栏能看到完整回调 URL，粘回面板即可——含 code 直接换 token，
//     含 secret+redirect 则写入 secret 并继续 ticket 轮询。
//  3. 反向代理暴露公网回调：CN_OAUTH_CALLBACK_HOST 显式带端口时改写链接
//     的 port 参数，让回调经反代进入本服务。
//
// 早期只有 cmd/cnlogin 的 -print-only 一条路，且没有本地回调服务就永远拿不到
// portal 下发的 secret（ticket 轮询必须用它），等于必然 5 分钟超时。这里把
// secret 的两种来源都补齐。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go-codearts/internal/cnauth"
	"go-codearts/internal/cnup"
)

const cnLoginTTL = 10 * time.Minute

type cnLoginSession struct {
	ID       string
	TicketID string
	Verifier string
	Port     int
	DPoP     cnup.DPoPPrivateJWK
	AuthURL  string
	Created  time.Time

	// mu 保护下面的可变字段：面板每 2 秒轮询状态，而 portal 回调在另一个
	// goroutine 里落结果，两边无锁读写会读到 Done=true 但 Account=nil，
	// 或让同一次登录被保存两遍。
	mu             sync.Mutex
	secret         string // 本地初始值；portal 回调后覆盖为上游认可的值
	done           bool
	errMsg         string
	ticketFallback bool
	account        map[string]any
}

// markDone 记录成功结果（再次调用无副作用）。
func (s *cnLoginSession) markDone(acct map[string]any) {
	s.mu.Lock()
	s.done, s.account, s.errMsg = true, acct, ""
	s.mu.Unlock()
}

// markErr 记录失败。
func (s *cnLoginSession) markErr(msg string) {
	s.mu.Lock()
	s.done, s.errMsg = true, msg
	s.mu.Unlock()
}

// snapshot 一次性读出状态，避免多次加锁读到互相矛盾的字段。
func (s *cnLoginSession) snapshot() (done bool, errMsg string, acct map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done, s.errMsg, s.account
}

// setSecret 记录 portal 下发的 secret 并开启 ticket 轮询通道。
func (s *cnLoginSession) setSecret(secret string) {
	s.mu.Lock()
	s.secret, s.ticketFallback = secret, true
	s.mu.Unlock()
}

// pollSecret 返回当前可用的 secret 与是否已开启轮询通道。
func (s *cnLoginSession) pollSecret() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secret, s.ticketFallback
}

// newSecretForSession 创建时写入本地 secret。
func (s *cnLoginSession) initSecret(secret string) {
	s.mu.Lock()
	s.secret = secret
	s.mu.Unlock()
}

type cnLoginStore struct {
	mu   sync.Mutex
	m    map[string]*cnLoginSession
	byTk map[string]*cnLoginSession
}

func newCNLoginStore() *cnLoginStore {
	return &cnLoginStore{m: map[string]*cnLoginSession{}, byTk: map[string]*cnLoginSession{}}
}

func (s *cnLoginStore) put(sess *cnLoginSession) {
	s.mu.Lock()
	s.gcLocked()
	s.m[sess.ID] = sess
	s.byTk[sess.TicketID] = sess
	s.mu.Unlock()
}

func (s *cnLoginStore) get(id string) *cnLoginSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	return s.m[id]
}

func (s *cnLoginStore) byTicket(tid string) *cnLoginSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byTk[tid]
}

func (s *cnLoginStore) drop(id string) {
	s.mu.Lock()
	if sess, ok := s.m[id]; ok {
		delete(s.byTk, sess.TicketID)
		delete(s.m, id)
	}
	s.mu.Unlock()
}

func (s *cnLoginStore) gcLocked() {
	cut := time.Now().Add(-cnLoginTTL)
	for id, sess := range s.m {
		if sess.Created.Before(cut) {
			delete(s.byTk, sess.TicketID)
			delete(s.m, id)
		}
	}
}

// cnListenPort 返回登记在授权链接里的回调端口。
// 优先用面板自己上报的端口（浏览器看到的是哪个端口就填哪个），
// 否则回退到服务监听端口——容器里端口映射时前者才准。
func (s *Server) cnListenPort(hint int) int {
	if hint > 0 && hint < 65536 {
		return hint
	}
	// cfg.Port 是形如 ":29137" 或 "29137" 的监听串，解析出端口做兜底。
	if p := portFromListen(s.cfg.Port); p > 0 {
		return p
	}
	return 29137
}

// rewriteAuthURLPort 改写授权链接的 port 参数（portal 据此拼回调地址）。
func rewriteAuthURLPort(authURL string, port int) string {
	u, err := url.Parse(authURL)
	if err != nil {
		return authURL
	}
	q := u.Query()
	q.Set("port", strconv.Itoa(port))
	u.RawQuery = q.Encode()
	return u.String()
}

// portOfCallbackHost 取 CN_OAUTH_CALLBACK_HOST 里显式写出的端口（0 = 不启用）。
// 只认显式端口，不按 scheme 猜 443，避免把本机回调用例改坏。
func portOfCallbackHost(host string) int {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return 0
	}
	if u, err := url.Parse(trimmed); err == nil && u.Host != "" {
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				return n
			}
		}
		return 0
	}
	if i := strings.LastIndex(trimmed, ":"); i >= 0 {
		seg := strings.TrimRight(trimmed[i+1:], "/")
		if n, err := strconv.Atoi(seg); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func loopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// cnLoginStart 生成一次授权会话。
func (s *Server) handleCNLoginStart(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.CNEnabled {
		writeErr(w, http.StatusBadRequest, "CN 渠道未启用（CN_ENABLED=false）")
		return
	}
	var req struct {
		Port int `json:"port"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	_ = json.Unmarshal(raw, &req)

	lcfg := cnup.DefaultLoginConfig()
	ticketID, err := cnup.RandomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	secret, err := cnup.RandomHex(16)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	verifier, challenge, err := cnup.PKCE()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dpopJWK, err := cnup.NewDPoPPrivateJWK()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	port := s.cnListenPort(req.Port)
	authURL := s.cnUpstream().BuildAuthorizeURL(lcfg, ticketID, challenge, "SHA-256", port)
	// 反代场景：显式配了端口才改写，让 portal 回调指向公网入口。
	if p := portOfCallbackHost(s.cfg.CNOAuthCallbackHost); p != 0 {
		authURL = rewriteAuthURLPort(authURL, p)
		port = p
	}

	// 会话 ID 与会话内 ticket 都不可预测；面板侧只凭 ID 查询。
	sessID, err := cnup.RandomHex(12)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sess := &cnLoginSession{
		ID: sessID, TicketID: ticketID, Verifier: verifier,
		Port: port, DPoP: dpopJWK, AuthURL: authURL, Created: time.Now(),
	}
	sess.initSecret(secret)
	s.cnLogins.put(sess)

	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sessID,
		"auth_url":   authURL,
		"port":       port,
		"expires_in": int(cnLoginTTL.Seconds()),
		"hint": "同机浏览器：点开链接登录，完成后本页会自动刷新。" +
			"跨机器：登录后浏览器会跳到 127.0.0.1 打不开，把地址栏那条 URL 整段粘到下方“粘贴回调地址”。",
	})
}

// cnLoginResult 把 token 落盘并入库，返回面板展示信息。
func (s *Server) saveCNLogin(tok *cnup.TokenResponse, verifier string, dpop cnup.DPoPPrivateJWK) (map[string]any, error) {
	if tok == nil || tok.Credentials.SecurityToken == "" {
		return nil, fmt.Errorf("上游未返回 STS 凭证")
	}
	rec := &cnauth.File{
		UserID:          tok.UserID,
		UserName:        tok.UserName,
		DomainID:        tok.DomainID,
		CloudDragonTok:  tok.Credentials.SecurityToken,
		AccessKeyID:     tok.Credentials.AccessKeyID,
		SecretAccessKey: tok.Credentials.SecretAccessKey,
		Expiration:      tok.Credentials.Expiration,
		RefreshToken:    tok.RefreshToken,
		CodeVerifier:    verifier,
		UpdatedAt:       time.Now().Unix(),
		ClientID:        cnup.DefaultLoginConfig().ClientID,
		DPoPPrivateKey:  map[string]string(dpop),
	}
	if s.cfg.CNAuthDir != "" {
		if _, err := cnauth.Save(s.cfg.CNAuthDir, rec); err != nil {
			return nil, fmt.Errorf("写 auths 失败: %w", err)
		}
	}
	// 直接入库，不必等定时导入——面板点完就能看到账号。
	if s.cfg.CNAuthDir != "" {
		if _, err := s.db.ImportCNAuthDir(s.cfg.CNAuthDir); err != nil {
			return nil, fmt.Errorf("导入入库失败: %w", err)
		}
		s.SyncShardFromDB()
	}
	return map[string]any{
		"uid": tok.UserID, "name": tok.UserName,
		"expires": tok.Credentials.Expiration,
	}, nil
}

// handleCNLoginStatus 轮询登录结果；pending 时顺带尝试 ticket 通道。
func (s *Server) handleCNLoginStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	sess := s.cnLogins.get(id)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired", "message": "会话已过期，请重新发起登录"})
		return
	}
	if done, errMsg, acct := sess.snapshot(); done {
		if errMsg != "" {
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": errMsg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "done", "message": "登录成功", "account": acct})
		return
	}
	// 只有 portal 明确下发过 secret（回环回调或粘贴回调）才能走 ticket 通道；
	// 否则本地 secret 上游不认，轮询只会白白撞墙。
	secret, fallback := sess.pollSecret()
	if !fallback {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "pending", "message": "等待浏览器完成授权…",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	tok, err := s.cnUpstream().PollTicket(ctx, cnup.DefaultLoginConfig(), sess.TicketID, secret)
	if err != nil || tok == nil || tok.UserName == "" || tok.Credentials.SecurityToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "pending", "message": "上游还没放行，稍候自动重试…",
		})
		return
	}
	acct, err := s.saveCNLogin(tok, sess.Verifier, sess.DPoP)
	if err != nil {
		sess.markErr(err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
		return
	}
	sess.markDone(acct)
	writeJSON(w, http.StatusOK, map[string]any{"status": "done", "message": "登录成功，账号已入库", "account": acct})
}

// handleCNLoginCallbackPaste 处理用户粘回来的回调地址。
//
// 跨机器场景：浏览器在操作员自己的电脑上，portal 回调该机器的 127.0.0.1，
// 打不开是正常的——地址栏里那条 URL 就是全部所需信息，粘回来即可。
func (s *Server) handleCNLoginCallbackPaste(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"session_id"`
		CallbackURL string `json:"callback_url"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	_ = json.Unmarshal(raw, &req)
	// 会话 ID 优先从 query 取（面板走这条），body 里带也兼容。
	if id := r.URL.Query().Get("id"); id != "" {
		req.SessionID = id
	}
	if req.SessionID == "" || strings.TrimSpace(req.CallbackURL) == "" {
		writeErr(w, http.StatusBadRequest, "session_id 与 callback_url 必填")
		return
	}
	sess := s.cnLogins.get(req.SessionID)
	if sess == nil {
		writeErr(w, http.StatusBadRequest, "会话不存在或已过期，请重新发起登录")
		return
	}
	cb, err := url.Parse(strings.TrimSpace(req.CallbackURL))
	if err != nil || cb.Scheme != "http" || cb.User != nil || !loopbackHost(cb.Hostname()) {
		writeErr(w, http.StatusBadRequest, "只接受华为返回的 127.0.0.1 回调地址")
		return
	}
	// 端口必须对得上会话，否则是另一次登录的残留地址。
	if p := cb.Port(); p != "" && p != strconv.Itoa(sess.Port) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("回调端口 %s 与本次授权（%d）不一致", p, sess.Port))
		return
	}
	q := cb.Query()
	if code := q.Get("code"); code != "" {
		if err := s.cnApplyCode(r, sess, code); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		_, _, acct := sess.snapshot()
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "done", "message": "OAuth 登录成功，账号已入库", "account": acct,
		})
		return
	}
	nextURL, err := s.cnApplySecret(sess, q.Get("secret"), q.Get("redirect"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "continue", "next_url": nextURL,
		"message": "回调已接收，请在浏览器继续完成授权，本页会自动检测结果",
	})
}

// cnApplySecret 接受 portal 下发的 secret。
//
// portal 第一次回调只带 secret + redirect：必须把浏览器 307 送回 redirect，
// 否则带 code 的第二次回调永远不会发生，登录就断在第一步。这也是旧
// -print-only 模式的死因——它没有服务可以去接这一次回调。
func (s *Server) cnApplySecret(sess *cnLoginSession, secret, nextURL string) (string, error) {
	next, nerr := url.Parse(nextURL)
	portal, perr := url.Parse(cnup.PortalHost)
	if secret == "" || nerr != nil || perr != nil || next.Scheme != "https" ||
		next.User != nil || !strings.EqualFold(next.Host, portal.Host) ||
		next.Query().Get("ticket_id") != sess.TicketID {
		return "", fmt.Errorf("回调地址缺少本次授权的 secret / redirect 信息")
	}
	sess.setSecret(secret)
	return nextURL, nil
}

// cnApplyCode 用授权码换 token 并落盘入库。
func (s *Server) cnApplyCode(r *http.Request, sess *cnLoginSession, code string) error {
	lcfg := cnup.DefaultLoginConfig()
	tok, err := s.cnUpstream().ExchangeCode(r.Context(), lcfg, code, sess.Verifier, sess.Port, sess.DPoP)
	if err != nil {
		msg := "换取 token 失败: " + err.Error()
		sess.markErr(msg)
		return fmt.Errorf("%s", msg)
	}
	acct, err := s.saveCNLogin(tok, sess.Verifier, sess.DPoP)
	if err != nil {
		sess.markErr(err.Error())
		return err
	}
	sess.markDone(acct)
	return nil
}

// handleCNOAuthCallback 接 portal 的 127.0.0.1 回环回调（同机场景）。
// 该路径与国际版共用，按 query 特征分流；没有 CN 会话时退回提示页。
func (s *Server) handleCNOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sess := s.cnLogins.byTicket(q.Get("ticket_id"))
	if sess == nil && q.Get("secret") != "" {
		// portal 首次回调可能不带 ticket_id，退回"唯一活跃会话"判定。
		sess = s.cnLogins.onlyActive()
	}
	if sess == nil {
		s.serveOAuthPlaceholder(w, r)
		return
	}

	// 第一步：只有 secret。必须 307 回 redirect，portal 才会继续下发 code。
	if q.Get("code") == "" {
		nextURL, err := s.cnApplySecret(sess, q.Get("secret"), q.Get("redirect"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, nextURL, http.StatusTemporaryRedirect)
		return
	}

	// 第二步：带 code，换 token 后给用户一个可读的结果页。
	_ = s.cnApplyCode(r, sess, q.Get("code"))
	s.cnCallbackPage(w, sess)
}

// onlyActive 在恰好只有一个未完成会话时返回它（portal 回调不带 ticket_id 的兜底）。
func (s *cnLoginStore) onlyActive() *cnLoginSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *cnLoginSession
	for _, sess := range s.m {
		if done, _, _ := sess.snapshot(); done {
			continue
		}
		if found != nil {
			return nil
		}
		found = sess
	}
	return found
}

func (s *Server) cnCallbackPage(w http.ResponseWriter, sess *cnLoginSession) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	done, errMsg, acct := sess.snapshot()
	switch {
	case done && errMsg == "":
		name := ""
		if acct != nil {
			if v, ok := acct["name"].(string); ok {
				name = v
			}
		}
		fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>登录成功</title></head>
<body style="font-family:system-ui;padding:40px;max-width:640px">
<h2>国内码道登录成功</h2><p>账号：%s</p><p>凭据已入库，可以关闭此页面回到面板。</p>
</body></html>`, htmlEscape(name))
	case errMsg != "":
		fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>登录失败</title></head>
<body style="font-family:system-ui;padding:40px;max-width:640px">
<h2>登录失败</h2><p>%s</p><p>回到面板重试，或把本页地址整段粘贴到「粘贴回调地址」。</p>
</body></html>`, htmlEscape(errMsg))
	default:
		fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8"><title>授权进行中</title></head>
<body style="font-family:system-ui;padding:40px;max-width:640px">
<h2>已接收华为授权回调</h2><p>正在等待上游放行凭据，请回到面板查看状态。</p>
<p>若这步之后浏览器仍停在华为页面，请继续完成授权。</p>
</body></html>`)
	}
}

// serveOAuthPlaceholder 是国际版 OAuth 的提示页（无 CN 会话时）。
func (s *Server) serveOAuthPlaceholder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html><body style="font-family:system-ui;padding:40px">
<h2>请用本地回调完成授权</h2>
<p>CodeArts OAuth 必须回调到 127.0.0.1。请回到面板重新发起授权（国际版走 CLI 授权页，国内码道走「国内码道」页）。</p>
</body></html>`)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// portFromListen 从 ":29137" / "0.0.0.0:29137" / "29137" 取出端口号。
func portFromListen(listen string) int {
	trimmed := strings.TrimSpace(listen)
	if trimmed == "" {
		return 0
	}
	if i := strings.LastIndex(trimmed, ":"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	if n, err := strconv.Atoi(trimmed); err == nil && n > 0 {
		return n
	}
	return 0
}
