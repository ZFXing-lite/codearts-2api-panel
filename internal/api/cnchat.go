package api

// CN channel dispatch: routes accounts with Channel=="cn" (国内码道/盘古助手)
// to the cnup upstream instead of the international snap pipeline. Kept in a
// separate file so the big handleHuaweiChat retry loop only gains one hook.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"go-codearts/internal/cnup"
	"go-codearts/internal/store"
)

// handleCNChat serves one chat attempt against a CN-channel account.
// Returns (committed, retry, ok):
//   - committed=true: response written（成功或最终错误），调用方必须收笔。
//     ok 告诉调用方这一次算真成功还是失败，用于 recordStats。
//   - retry=true: 瞬态失败，可换账号再试；但是槽位满不消耗重试预算
//     （slotFull 由 CommittedTagSlotFull 区分）。
//   - retry=false & committed=false: 本地失败（如 body 错误已 writeErr）。
func (s *Server) handleCNChat(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, acc *store.Account, wantStream bool, proto clientProto, model string) (committed, retry, ok, slotFull bool) {
	release := s.cnAcquire(acc.ID)
	if release == nil {
		// 本地并发槽满 ≠ 上游失败，不计入重试预算（由 slotFull 标记）
		return false, true, false, true
	}
	defer release()
	cnStarted := time.Now()

	if acc.Metadata["cn_needs_relogin"] == "true" {
		return false, true, false, false
	}
	// Proactive refresh inside the skew window, same contract as cnwatch.
	if !acc.TokenExpires.IsZero() && time.Until(acc.TokenExpires) < s.cfg.CNRefreshSkew {
		if err := s.cnRefresh(acc); err != nil {
			if s.cnMarkReloginErr(acc, err) {
				return false, true, false, false
			}
			log.Printf("cn chat refresh rid=%s account=%s err=%v", reqIDFromRequest(r), acc.ID, err)
			// fall through with the old token; upstream will 401 if truly dead
		}
	}

	chatID := cnChatID(r, body, acc)
	msgs, opts, err := cnConvertBody(body)
	if err != nil {
		// 请求体级错误（缺 messages 等）轮转到任何账号都一样失败，
		// 直接向客户端吐 400，不消耗轮转配额。
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("cn body: %v", err))
		return true, false, false, false
	}
	benefit := cnup.IsBenefitModel(acc.UserID, model)
	traceID, _ := cnup.RandomHex(16)
	cred := cnup.SignCredential{SecurityToken: acc.AccessToken}
	if acc.Credentials != nil {
		cred.AccessKeyID = acc.Credentials.AccessKeyID
		cred.SecretAccessKey = acc.Credentials.SecretAccessKey
	}

	rc, err := s.cnUpstream().ChatStreamWithOptions(ctx, chatID, msgs, traceID, cred, acc.Email, model, opts, benefit)
	if err != nil {
		// CN 之前完全绕过熔断与失败计数：坏账号被反复首选，面板上的
		// SuccessCount/FailCount 恒为 0。这里补齐，与国际版对齐。
		if s.circuit != nil {
			s.circuit.RecordFailureWithType(acc.ID, classifyError(0, err.Error()), 0)
		}
		if s.cnMarkReloginErr(acc, err) || cnIsAuthErr(err) {
			s.recordAccountFail(acc, err.Error())
			s.cnCooldownIfNeeded(acc)
			return false, true, false, false
		}
		if isTransientUpstreamErr(err) || errors.Is(err, context.DeadlineExceeded) {
			s.recordAccountRotate(acc, "cn upstream timeout (rotated)")
			s.markAccountSessionBusy(acc.ID, s.cfg.CNSoftCooldown)
			return false, true, false, false
		}
		if ctx.Err() != nil {
			return true, false, false, false // client gone; nothing sensible left to write
		}
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("cn upstream: %v", err))
		return true, false, false, false
	}
	defer rc.Close()

	s.cnTouchActive(acc)
	if s.circuit != nil {
		s.circuit.RecordSuccess(acc.ID)
	}

	// usage 必须接出来：CN 此前全程不记账，账号的 SuccessCount / TokenUsed /
	// LastLatency / Depleted() 永远停在导入初值，面板显示"从未使用"，
	// 选号也无法按真实余额倾斜；按 key 的 token 配额同样记 0。
	var cnPrompt, cnCompletion, cnTotal float64
	if wantStream && proto != protoChat {
		// CN 上游只会吐 OpenAI 格式 SSE。Claude(/v1/messages) 与
		// Responses(/v1/responses) 客户端要的是各自的帧序列，之前这里
		// 直接返回一次性 application/json，SDK 会报
		// "stream ended without message_start" —— 该渠道对这两个协议整体不可用。
		// 这里先聚合、再按目标协议回放：帧序列合法，代价是整段生成完才发。
		cnAgg, cnErr := cnup.AggregateRaw(rc)
		if cnErr != nil {
			log.Printf("cn chat aggregate rid=%s account=%s err=%v", reqIDFromRequest(r), acc.ID, cnErr)
			return true, false, false, false
		}
		cnPrompt, cnCompletion, cnTotal = cnUsageTokens(cnAgg)
		s.recordAccountSuccessFull(acc, cnTotal, cnPrompt, cnCompletion, time.Since(cnStarted).Milliseconds())
		cnBody, _ := json.Marshal(cnup.RenderOpenAI(cnAgg, model))
		writeOpenAIAsProtoSSE(w, cnBody, &ssePin{Proto: proto, Model: model}, false)
		return true, false, true, false
	}
	if wantStream && proto == protoChat {
		serr := cnup.StreamCaptureWithUsage(w, rc, model, func(fin *cnup.RawCompletion) {
			cnPrompt, cnCompletion, cnTotal = cnUsageTokens(fin)
		}, func(fin *cnup.RawCompletion) map[string]any {
			p, comp, tot := cnUsageTokens(fin)
			return map[string]any{
				"prompt_tokens": p, "completion_tokens": comp, "total_tokens": tot,
			}
		})
		if serr != nil {
			log.Printf("cn chat stream rid=%s account=%s mid-stream err=%v", reqIDFromRequest(r), acc.ID, serr)
		}
		if serr == nil {
			s.recordAccountSuccessFull(acc, cnTotal, cnPrompt, cnCompletion, time.Since(cnStarted).Milliseconds())
		}
		return true, false, serr == nil, false
	}

	aggRaw, aggErr := cnup.AggregateRaw(rc)
	if aggErr != nil {
		if s.cnMarkReloginErr(acc, aggErr) {
			return false, true, false, false
		}
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("cn upstream aggregate: %v", aggErr))
		return true, false, false, false
	}
	cnPrompt, cnCompletion, cnTotal = cnUsageTokens(aggRaw)
	s.recordAccountSuccessFull(acc, cnTotal, cnPrompt, cnCompletion, time.Since(cnStarted).Milliseconds())
	agg := cnup.RenderOpenAI(aggRaw, model)
	raw, _ := json.Marshal(agg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encodeProtoBody(proto, raw, &ssePin{Proto: proto, Model: model}))
	return true, false, true, false
}

// rewriteBodyModel 复写请求体顶层 model 字段（剥渠道前缀用）。
func rewriteBodyModel(body []byte, model string) []byte {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil || root == nil {
		return body
	}
	if _, ok := root["model"]; !ok {
		return body
	}
	root["model"] = model
	raw, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return raw
}

func (s *Server) cnUpstream() *cnup.Client {
	s.cnOnce.Do(func() {
		s.cnClient = cnup.New(s.cfg.CNUpstreamTimeout)
		s.cnClient.SetBenefitAutoClaim(s.cfg.CNBenefitAutoClaim)
	})
	return s.cnClient
}

// cnAcquire takes a per-account slot honoring cfg.CNMaxConcurrent.
// Returns nil when the account is at capacity.
func (s *Server) cnAcquire(accountID string) func() {
	max := s.cfg.CNMaxConcurrent
	if max <= 0 {
		max = 5
	}
	s.cnSemaMu.Lock()
	if s.cnSema == nil {
		s.cnSema = map[string]chan struct{}{}
	}
	ch := s.cnSema[accountID]
	if ch == nil {
		ch = make(chan struct{}, max)
		s.cnSema[accountID] = ch
	}
	s.cnSemaMu.Unlock()
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-ch }) }
	default:
		return nil
	}
}

// cnChatID resolves the CN conversation id: explicit header / body field beats
// the account's sticky ChatID; a fresh id is minted when neither exists.
func cnChatID(r *http.Request, body []byte, acc *store.Account) string {
	if v := strings.TrimSpace(r.Header.Get("X-Codearts-Chat-Id")); v != "" {
		return v
	}
	var payload struct {
		ConversationID string `json:"conversation_id"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.ConversationID != "" {
		return payload.ConversationID
	}
	if acc.ChatID != "" {
		return acc.ChatID
	}
	id, err := cnup.RandomHex(16)
	if err != nil {
		return fmt.Sprintf("cn-%d", time.Now().UnixNano())
	}
	return "cn-" + id
}

// cnConvertBody maps an OpenAI chat.completions body to cnup messages/options.
// Claude Messages / Responses callers hit the same body shape here because the
// CN branch runs after sanitizeCLIChatBody normalization.
func cnConvertBody(body []byte) ([]cnup.ChatMessage, cnup.ChatOptions, error) {
	var req struct {
		Messages []struct {
			Role       string              `json:"role"`
			Content    json.RawMessage     `json:"content"`
			ToolCalls  []cnup.ChatToolCall `json:"tool_calls"`
			ToolCallID string              `json:"tool_call_id"`
			Name       string              `json:"name"`
		} `json:"messages"`
		MaxTokens   *int             `json:"max_tokens"`
		Temperature *float64         `json:"temperature"`
		TopP        *float64         `json:"top_p"`
		Tools       []map[string]any `json:"tools"`
		ToolChoice  any              `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, cnup.ChatOptions{}, err
	}
	msgs := make([]cnup.ChatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		cm := cnup.ChatMessage{
			Role:       m.Role,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if len(m.Content) > 0 {
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				cm.Content = s
			} else {
				// multipart content: keep only text parts; images are not
				// supported by the CN channel.
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(m.Content, &parts); err == nil {
					var sb strings.Builder
					for _, p := range parts {
						if p.Type == "text" {
							sb.WriteString(p.Text)
						}
					}
					cm.Content = sb.String()
				} else {
					cm.Content = string(m.Content)
				}
			}
		}
		msgs = append(msgs, cm)
	}
	if len(msgs) == 0 {
		return nil, cnup.ChatOptions{}, fmt.Errorf("messages missing")
	}
	return msgs, cnup.ChatOptions{
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
	}, nil
}

// cnRefresh runs one inline token refresh when the chat path notices the
// access token inside the skew window. Mirrors cnwatch's write-back contract.
func (s *Server) cnRefresh(acc *store.Account) error {
	unlock := s.db.RefreshLock(acc.ID)
	defer unlock()
	// cnwatch 可能已抢先完成本轮刷新，重读活账号比对 refresh token
	if fresh, err := s.db.Get(acc.ID); err == nil && fresh != nil && fresh.RefreshToken != acc.RefreshToken {
		return nil // 已被另一侧刷新，放弃本次避免双轮换自毁
	}
	loginCfg := cnup.DefaultLoginConfig()
	if v := acc.Metadata["cn_client_id"]; v != "" {
		loginCfg.ClientID = v
	} else if v := acc.Metadata["client_id"]; v != "" {
		loginCfg.ClientID = v
	}
	var dpop cnup.DPoPPrivateJWK
	if raw := acc.Metadata["cn_dpop_private_key"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &dpop)
	}
	// 刷新必须带超时：客户端断开了也要在 CN_UPSTREAM_TIMEOUT 内收敛，
	// 否则会一边跑满 120s 一边长期占着 RefreshLock，把 cnwatch 的续期
	// 和其它请求的 inline 刷新全部堵在队列里。
	timeout := s.cfg.CNUpstreamTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tr, err := s.cnUpstream().RefreshToken(ctx, loginCfg,
		acc.RefreshToken, acc.Metadata["cn_code_verifier"], dpop)
	if err != nil {
		return err
	}
	// 用 Mutate 原子改活账号：Get→改→Upsert 两步之间可能与其他写者竞争。
	return s.db.Mutate(acc.ID, func(fresh *store.Account) error {
		applyCNTokenResponse(fresh, tr)
		return nil
	})
}

func applyCNTokenResponse(acc *store.Account, tr *cnup.TokenResponse) {
	if tr == nil {
		return
	}
	// CN 渠道的 access token 即 STS security_token（cloud_dragon_token）。
	if tr.Credentials.SecurityToken != "" {
		acc.AccessToken = tr.Credentials.SecurityToken
	} else if tr.Credential.SecurityToken != "" {
		acc.AccessToken = tr.Credential.SecurityToken
	}
	if tr.RefreshToken != "" {
		acc.RefreshToken = tr.RefreshToken
	}
	if tr.Credentials.Expiration != "" {
		acc.TokenExpires = store.ParseFlexibleTime(tr.Credentials.Expiration)
	} else if tr.Credential.ExpiresAt != "" {
		acc.TokenExpires = store.ParseFlexibleTime(tr.Credential.ExpiresAt)
	}
	// AK/SK 也一并回写（原逻辑要求 Credentials 非 nil 才更新，
	// 刷新时会有新 AK/SK 上来但永远写不进去——cnwatch 对齐版本）
	if tr.Credentials.AccessKeyID != "" && tr.Credentials.SecretAccessKey != "" {
		acc.Credentials = &store.Credentials{
			AccessKeyID:     tr.Credentials.AccessKeyID,
			SecretAccessKey: tr.Credentials.SecretAccessKey,
			SecurityToken:   acc.AccessToken,
			Expiration:      acc.TokenExpires,
			AuthMode:        "oauth_sts",
		}
	} else if acc.Credentials != nil && acc.AccessToken != "" {
		acc.Credentials.SecurityToken = acc.AccessToken
		acc.Credentials.Expiration = acc.TokenExpires
	}
}

// cnMarkReloginErr flags the account when the refresh token is dead.
// Returns true when the error was of that class.
// cnUsageTokens 从上游 usage 报文中取三类 token 数（缺失时返回 0）。
func cnUsageTokens(rc *cnup.RawCompletion) (prompt, completion, total float64) {
	if rc == nil || rc.Usage == nil {
		return 0, 0, 0
	}
	num := func(k string) float64 {
		switch v := rc.Usage[k].(type) {
		case float64:
			return v
		case float32:
			return float64(v)
		case int:
			return float64(v)
		case int64:
			return float64(v)
		case json.Number:
			f, _ := v.Float64()
			return f
		}
		return 0
	}
	prompt = num("prompt_tokens")
	if prompt == 0 {
		prompt = num("input_tokens")
	}
	completion = num("completion_tokens")
	if completion == 0 {
		completion = num("output_tokens")
	}
	total = num("total_tokens")
	if total == 0 {
		total = prompt + completion
	}
	return prompt, completion, total
}

// cnCooldownIfNeeded 让 CN_ERR_THRESHOLD / CN_ERR_COOLDOWN 真正生效：
// 连续失败达到阈值就把账号晾一段时间，别让每个新请求都先去撞一次坏账号。
func (s *Server) cnCooldownIfNeeded(acc *store.Account) {
	if s == nil || acc == nil {
		return
	}
	threshold := int64(s.cfg.CNErrThreshold)
	if threshold <= 0 {
		threshold = 3
	}
	// applyAccountMutation 会把累计值回写到调用方副本上
	if acc.FailCount < threshold {
		return
	}
	d := s.cfg.CNErrCooldown
	if d <= 0 {
		d = 10 * time.Minute
	}
	s.markAccountSessionBusy(acc.ID, d)
	log.Printf("cn cooldown account=%s fails=%d for=%s", acc.ID, acc.FailCount, d)
}
func (s *Server) cnMarkReloginErr(acc *store.Account, err error) bool {
	var rte *cnup.RefreshTokenExpiredError
	if !errors.As(err, &rte) {
		return false
	}
	if merr := s.db.Mutate(acc.ID, func(fresh *store.Account) error {
		if fresh.Metadata == nil {
			fresh.Metadata = map[string]string{}
		}
		fresh.Metadata["cn_needs_relogin"] = "true"
		fresh.LastError = "cn refresh token expired; re-login via cnlogin"
		return nil
	}); merr != nil {
		log.Printf("cn mark relogin account=%s err=%v", acc.ID, merr)
	}
	return true
}

func cnIsAuthErr(err error) bool {
	var ue *cnup.UpstreamError
	if !errors.As(err, &ue) {
		return false
	}
	msg := strings.ToLower(ue.Msg + " " + ue.Code)
	return strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "invalid token") || strings.Contains(msg, "token expired")
}

// cnSplitChannelModel 解析模型名的渠道前缀路由（借鉴 workbuddy2api 的
// realm 前缀协议）："cn:<model>" 强制只选 CN 渠道账号，"intl:<model>"
// 强制只选国际账号；无前缀保持现状双亲调度。返回 (realm, bare)。
// realm ∈ {"", "cn", "intl"}。
func cnSplitChannelModel(model string) (realm, bare string) {
	if i := strings.Index(model, ":"); i > 0 {
		switch model[:i] {
		case store.ChannelCN:
			return store.ChannelCN, model[i+1:]
		case store.ChannelIntl, "global":
			return store.ChannelIntl, model[i+1:]
		}
	}
	return "", model
}

// filterAccountsByRealm 按渠道过滤候选账号集合；realm=="" 原样返回
// （零改动语义，同 workbuddy PickExcludingForRealm 的退化分支）。
func filterAccountsByRealm(accounts []*store.Account, realm string) []*store.Account {
	if realm == "" {
		return accounts
	}
	out := accounts[:0:0]
	for _, a := range accounts {
		if a != nil && store.NormalizeChannel(a.Channel) == realm {
			out = append(out, a)
		}
	}
	return out
}

// cnTouchActive records chat activity for cnwatch keepalive gating.
func (s *Server) cnTouchActive(acc *store.Account) {
	_ = s.db.Mutate(acc.ID, func(fresh *store.Account) error {
		if fresh.Metadata == nil {
			fresh.Metadata = map[string]string{}
		}
		fresh.Metadata["cn_last_active"] = time.Now().Format(time.RFC3339)
		return nil
	})
}
