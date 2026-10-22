// Package cnauth 是国内码道（华为云 CodeArts / 盘古助手）OAuth 凭证的落盘格式。
//
// CLI（cmd/cnlogin）、网关导入（internal/store）与面板内置登录
// （internal/api/cnlogin.go）共用这一份定义，避免三处 schema 漂移：
// 任何一方漏写 refresh_token / code_verifier / dpop_private_key，
// 续期都会在 STS 侧被拒（InvalidDPoPHeader）。
package cnauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File 对应 auths/codearts-{user_id}.json。
type File struct {
	UserID          string            `json:"user_id"`
	UserName        string            `json:"user_name"`
	DomainID        string            `json:"domain_id"`
	CloudDragonTok  string            `json:"cloud_dragon_token"` // = STS security_token，即 x-auth-token
	AccessKeyID     string            `json:"access_key_id"`
	SecretAccessKey string            `json:"secret_access_key"`
	Expiration      string            `json:"expiration"` // RFC3339
	RefreshToken    string            `json:"refresh_token"`
	CodeVerifier    string            `json:"code_verifier"` // PKCE verifier，refresh 需要
	UpdatedAt       int64             `json:"updated_at"`
	ClientID        string            `json:"client_id,omitempty"`
	DPoPPrivateKey  map[string]string `json:"dpop_private_key,omitempty"`
	// ChatID 不是上游登录负载的一部分，仅运维手工固定会话时出现。
	ChatID string `json:"chat_id,omitempty"`
}

// SafeID 把上游给的 user_id 收敛成可用作文件名与账号 ID 的形态。
// user_id 来自上游响应，直接拼路径就是路径穿越（可以写出 auth 目录）。
func SafeID(userID string) string {
	var b strings.Builder
	for _, r := range userID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return ""
	}
	return out
}

// FileName 返回该账号的凭证文件名。
func FileName(userID string) string { return "codearts-" + SafeID(userID) + ".json" }

// IsAuthFile 判断目录项是否为登录产物。
func IsAuthFile(name string) bool {
	return strings.HasPrefix(name, "codearts-") && strings.HasSuffix(name, ".json")
}

// Save 写盘：临时文件 + rename，避免半写文件被导入方读到。
func Save(dir string, f *File) (string, error) {
	if SafeID(f.UserID) == "" {
		return "", fmt.Errorf("invalid user_id %q", f.UserID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if f.UpdatedAt == 0 {
		f.UpdatedAt = time.Now().Unix()
	}
	path := filepath.Join(dir, FileName(f.UserID))
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}
