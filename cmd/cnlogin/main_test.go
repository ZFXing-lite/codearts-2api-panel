package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-codearts/internal/cnauth"
	"go-codearts/internal/cnup"
)

// 文件名生成对齐 codearts2api 约定：codearts-{user_id}.json。
func TestFileName(t *testing.T) {
	if got := cnauth.FileName("u123"); got != "codearts-u123.json" {
		t.Fatalf("fileName = %q", got)
	}
}

// PKCE：verifier 非空且 challenge 是 verifier 的 S256（verifier != challenge）。
func TestPKCE(t *testing.T) {
	v, ch, err := cnup.PKCE()
	if err != nil {
		t.Fatalf("PKCE: %v", err)
	}
	if v == "" || ch == "" || v == ch {
		t.Fatalf("bad pkce: verifier=%q challenge=%q", v, ch)
	}
	v2, ch2, _ := cnup.PKCE()
	if v2 == v || ch2 == ch {
		t.Fatal("PKCE not random")
	}
}

// 授权链接拼接：包含 client_id/ticket_id/code_challenge/redirect_uri。
func TestBuildAuthorizeURL(t *testing.T) {
	c := cnup.New(0)
	cfg := cnup.DefaultLoginConfig()
	u := c.BuildAuthorizeURL(cfg, "tid123", "chall456", "SHA-256", 8080)
	for _, want := range []string{
		"client_id=" + cfg.ClientID,
		"ticket_id=tid123",
		"code_challenge=chall456",
		"code_challenge_method=SHA-256",
		"port=8080",
		cfg.PortalHost,
	} {
		if !strings.Contains(u, want) {
			t.Errorf("authorize url missing %q: %s", want, u)
		}
	}
}

// 落盘 schema：字段名与 codearts2api-cn/internal/auth 一致，且能 round-trip。
func TestSaveAuthSchema(t *testing.T) {
	dir := t.TempDir()
	a := &cnauth.File{
		UserID:          "uid1",
		UserName:        "n",
		DomainID:        "d",
		CloudDragonTok:  "tok",
		AccessKeyID:     "ak",
		SecretAccessKey: "sk",
		Expiration:      "2026-01-01T00:00:00Z",
		RefreshToken:    "rt",
		CodeVerifier:    "cv",
		UpdatedAt:       1,
		ClientID:        "cid",
		DPoPPrivateKey:  map[string]string{"kty": "EC"},
	}
	path, err := cnauth.Save(dir, a)
	if err != nil {
		t.Fatalf("cnauth.Save: %v", err)
	}
	if filepath.Base(path) != "codearts-uid1.json" {
		t.Fatalf("path = %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"user_id", "user_name", "domain_id", "cloud_dragon_token",
		"access_key_id", "secret_access_key", "expiration",
		"refresh_token", "code_verifier", "updated_at",
		"client_id", "dpop_private_key",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("schema missing %q", k)
		}
	}
	// 临时文件不应残留。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file left behind")
	}
}
