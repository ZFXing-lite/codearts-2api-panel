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
// Returns (committed, retry): committed=true means a response was written
// (success or final error) and the caller must end the request; retry=true
// means a transient/pool-level failure with nothing written — rotate account.
func (s *Server) handleCNChat(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, acc *store.Account, wantStream bool, proto clientProto, model string) (bool, bool) {
	release := s.cnAcquire(acc.ID)
	if release == nil {
		return false, true // per-account slots full — pick another account
	}
	defer release()

	if acc.Metadata["cn_needs_relogin"] == "true" {
		return false, true
	}
	// Proactive refresh inside the skew window, same contract as cnwatch.
	if !acc.TokenExpires.IsZero() && time.Until(acc.TokenExpires) < s.cfg.CNRefreshSkew {
		if err := s.cnRefresh(acc); err != nil {
			if s.cnMarkReloginErr(acc, err) {
				return false, true
			}
			log.Printf("cn chat refresh rid=%s account=%s err=%v", reqIDFromRequest(r), acc.ID, err)
			// fall through with the old token; upstream will 401 if truly dead
		}
	}

	chatID := cnChatID(r, body, acc)
	msgs, opts, err := cnConvertBody(body)
	if err != nil {
		return false, true // malformed shape — rotate; body-level 400s already handled upstream
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
		if s.cnMarkReloginErr(acc, err) || cnIsAuthErr(err) {
			return false, true
		}
		if isTransientUpstreamErr(err) || errors.Is(err, context.DeadlineExceeded) {
			return false, true
		}
		if ctx.Err() != nil {
			return true, false // client gone; nothing sensible left to write
		}
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("cn upstream: %v", err))
		return true, false
	}
	defer rc.Close()

	s.cnTouchActive(acc)

	if wantStream && proto == protoChat {
		if err := cnup.StreamCapture(w, rc, model, func(*cnup.RawCompletion) {}); err != nil {
			log.Printf("cn chat stream rid=%s account=%s mid-stream err=%v", reqIDFromRequest(r), acc.ID, err)
		}
		return true, false
	}

	agg, err := cnup.Aggregate(rc, model)
	if err != nil {
		if s.cnMarkReloginErr(acc, err) {
			return false, true
		}
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("cn upstream aggregate: %v", err))
		return true, false
	}
	raw, _ := json.Marshal(agg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encodeProtoBody(proto, raw, &ssePin{Proto: proto, Model: model}))
	return true, false
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
	tr, err := s.cnUpstream().RefreshToken(context.Background(), loginCfg,
		acc.RefreshToken, acc.Metadata["cn_code_verifier"], dpop)
	if err != nil {
		return err
	}
	fresh, err := s.db.Get(acc.ID)
	if err != nil || fresh == nil {
		return err
	}
	applyCNTokenResponse(fresh, tr)
	return s.db.Upsert(fresh)
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
	if acc.Credentials != nil && acc.AccessToken != "" {
		acc.Credentials.SecurityToken = acc.AccessToken
		if tr.Credentials.AccessKeyID != "" {
			acc.Credentials.AccessKeyID = tr.Credentials.AccessKeyID
			acc.Credentials.SecretAccessKey = tr.Credentials.SecretAccessKey
		}
	}
}

// cnMarkReloginErr flags the account when the refresh token is dead.
// Returns true when the error was of that class.
func (s *Server) cnMarkReloginErr(acc *store.Account, err error) bool {
	var rte *cnup.RefreshTokenExpiredError
	if !errors.As(err, &rte) {
		return false
	}
	if fresh, gerr := s.db.Get(acc.ID); gerr == nil && fresh != nil {
		if fresh.Metadata == nil {
			fresh.Metadata = map[string]string{}
		}
		fresh.Metadata["cn_needs_relogin"] = "true"
		fresh.LastError = "cn refresh token expired; re-login via cnlogin"
		_ = s.db.Upsert(fresh)
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
	fresh, err := s.db.Get(acc.ID)
	if err != nil || fresh == nil {
		return
	}
	if fresh.Metadata == nil {
		fresh.Metadata = map[string]string{}
	}
	fresh.Metadata["cn_last_active"] = time.Now().Format(time.RFC3339)
	_ = s.db.Upsert(fresh)
}
