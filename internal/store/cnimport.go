package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go-codearts/internal/cnauth"
)

// Channel values.
const (
	ChannelIntl = "intl" // 国际版存量账号（默认）
	ChannelCN   = "cn"   // 国内码道渠道
)

// NormalizeChannel returns the effective channel; empty legacy rows are intl.
// IsCNAccount 判定账号是否属于国内码道渠道。
//
// NormalizeChannel("") 一律返回 intl，于是 Channel 字段缺失的账号
// （手写 accounts.json、早期导入、跨版本迁移）即便带着 user_id 与
// cloud_dragon_token，也会被归到国际侧：模型列表显示成 global:、
// 调度把它派给国际 API，必然 401。这里用特征兜底，但显式声明过
// 别的渠道时不推翻它。
func IsCNAccount(a *Account) bool {
	if a == nil {
		return false
	}
	if NormalizeChannel(a.Channel) == ChannelCN {
		return true
	}
	if strings.TrimSpace(a.Channel) != "" {
		return false // 显式声明了其他渠道，尊重声明
	}
	// 国内登录产物必有 user_id，且 token 落在 AccessToken 或 Metadata 里。
	if strings.TrimSpace(a.UserID) == "" {
		return false
	}
	if strings.TrimSpace(a.AccessToken) != "" {
		return true
	}
	if a.Metadata != nil && strings.TrimSpace(a.Metadata["cloud_dragon_token"]) != "" {
		return true
	}
	return false
}

func NormalizeChannel(ch string) string {
	if strings.TrimSpace(ch) == "" {
		return ChannelIntl
	}
	return strings.TrimSpace(ch)
}

// cnAuth 复用 cnauth.File：CLI、网关、面板三方共享同一份落盘 schema。
type cnAuth = cnauth.File

// ImportCNAuthDir scans dir for codearts-*.json login artifacts produced by
// codearts2api and upserts them as Channel=cn accounts. Dedup is keyed by
// user_id first, then email/user_name: existing cn accounts are refreshed,
// existing intl accounts are never overwritten (skipped). Files missing
// user_id or cloud_dragon_token are skipped. Returns the number of upserts.
func (db *DB) ImportCNAuthDir(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	imported := 0
	var errs []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "codearts-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		var a cnAuth
		if err := json.Unmarshal(raw, &a); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		if a.UserID == "" || a.CloudDragonTok == "" {
			continue
		}
		ok, err := db.upsertCNAuth(&a)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		if ok {
			imported++
		}
	}
	if len(errs) > 0 {
		return imported, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return imported, nil
}

// upsertCNAuth converts one cnAuth into an Account. Returns false when the
// record collides with a non-cn account we must not clobber.
func (db *DB) upsertCNAuth(a *cnAuth) (bool, error) {
	var existing *Account
	for _, cand := range db.List() {
		if cand == nil {
			continue
		}
		if cand.UserID != "" && cand.UserID == a.UserID {
			existing = cand
			break
		}
		if a.UserName != "" && strings.EqualFold(strings.TrimSpace(cand.Email), strings.TrimSpace(a.UserName)) {
			existing = cand
			break
		}
	}
	var acc *Account
	if existing != nil {
		if NormalizeChannel(existing.Channel) != ChannelCN {
			// Belongs to the intl pool — never overwrite.
			return false, nil
		}
		cp, err := db.SnapshotAccount(existing.ID)
		if err != nil {
			return false, err
		}
		acc = cp
	} else {
		acc = &Account{
			ID:        "cn-" + a.UserID,
			Email:     strings.TrimSpace(a.UserName),
			CreatedAt: time.Now(),
		}
	}
	acc.Channel = ChannelCN
	acc.UserID = a.UserID
	acc.AccessToken = a.CloudDragonTok
	acc.ChatID = a.ChatID
	acc.RefreshToken = a.RefreshToken
	acc.TokenExpires = ParseFlexibleTime(a.Expiration)
	if a.AccessKeyID != "" && a.SecretAccessKey != "" {
		acc.Credentials = &Credentials{
			AccessKeyID:     a.AccessKeyID,
			SecretAccessKey: a.SecretAccessKey,
			SecurityToken:   a.CloudDragonTok,
			Expiration:      acc.TokenExpires,
			AuthMode:        "oauth_sts",
		}
	}
	acc.Opened = true
	acc.Status = "token_ok"
	if acc.Metadata == nil {
		acc.Metadata = map[string]string{}
	}
	if a.DomainID != "" {
		acc.Metadata["domain_id"] = a.DomainID
	}
	if a.ClientID != "" {
		acc.Metadata["cn_client_id"] = a.ClientID
	}
	if a.CodeVerifier != "" {
		acc.Metadata["cn_code_verifier"] = a.CodeVerifier
	}
	if len(a.DPoPPrivateKey) > 0 {
		if raw, err := json.Marshal(a.DPoPPrivateKey); err == nil {
			acc.Metadata["cn_dpop_private_key"] = string(raw)
		}
	}
	acc.Metadata["cn_imported_at"] = time.Now().Format(time.RFC3339)
	// 重新导入（cnlogin 再登录）视为凭证已续命，一并摘掉 relogin/错误状态
	delete(acc.Metadata, "cn_needs_relogin")
	acc.LastError = ""
	return true, db.Upsert(acc)
}
