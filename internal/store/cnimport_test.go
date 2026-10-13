package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCNAuth(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestImportCNAuthDir(t *testing.T) {
	dataDir := t.TempDir()
	db, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Save()

	// Pre-existing intl account sharing the email of one CN artifact:
	// it must NOT be overwritten or converted to cn.
	if err := db.Upsert(&Account{
		ID:      "acc-intl-1",
		Email:   "intluser@example.com",
		Channel: ChannelIntl,
	}); err != nil {
		t.Fatal(err)
	}

	authDir := t.TempDir()
	writeCNAuth(t, authDir, "codearts-u1.json", `{
		"user_id": "u1",
		"user_name": "cnuser1@example.com",
		"domain_id": "d1",
		"cloud_dragon_token": "tok-u1",
		"access_key_id": "ak-u1",
		"secret_access_key": "sk-u1",
		"expiration": "2099-01-01T00:00:00Z",
		"refresh_token": "rt-u1",
		"code_verifier": "cv-u1",
		"client_id": "cid-u1",
		"updated_at": 1700000000
	}`)
	writeCNAuth(t, authDir, "codearts-u2.json", `{
		"user_id": "u2",
		"user_name": "intluser@example.com",
		"cloud_dragon_token": "tok-u2",
		"refresh_token": "rt-u2",
		"updated_at": 1700000000
	}`)
	// Broken + incomplete files are tolerated/skipped.
	writeCNAuth(t, authDir, "codearts-bad.json", `{not json`)
	writeCNAuth(t, authDir, "codearts-notoken.json", `{"user_id": "uX"}`)
	writeCNAuth(t, authDir, "unrelated.json", `{"user_id": "zzz", "cloud_dragon_token": "x"}`)

	n, err := db.ImportCNAuthDir(authDir)
	if err == nil {
		t.Fatal("expected aggregated error for the broken file")
	}
	if n != 1 {
		t.Fatalf("imported = %d, want 1 (u2 collides with existing intl email)", n)
	}

	acc, err := db.Get("cn-u1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Channel != ChannelCN || acc.UserID != "u1" || acc.AccessToken != "tok-u1" ||
		acc.RefreshToken != "rt-u1" || acc.Email != "cnuser1@example.com" {
		t.Fatalf("imported account mismatch: %#v", acc)
	}
	if acc.TokenExpires.IsZero() || acc.TokenExpires.Before(time.Now()) {
		t.Fatalf("token_expires_at not parsed: %v", acc.TokenExpires)
	}
	if acc.Credentials == nil || acc.Credentials.AccessKeyID != "ak-u1" || acc.Credentials.SecretAccessKey != "sk-u1" || acc.Credentials.SecurityToken != "tok-u1" {
		t.Fatalf("credentials mismatch: %#v", acc.Credentials)
	}

	// intl account untouched.
	intl, err := db.Get("acc-intl-1")
	if err != nil {
		t.Fatal(err)
	}
	if intl.Channel != ChannelIntl || intl.AccessToken == "tok-u2" || intl.RefreshToken == "rt-u2" {
		t.Fatalf("intl account was overwritten: %#v", intl)
	}

	// Re-import dedups by user_id and refreshes the same cn account (no dup).
	writeCNAuth(t, authDir, "codearts-u1.json", `{
		"user_id": "u1",
		"user_name": "cnuser1@example.com",
		"cloud_dragon_token": "tok-u1-b",
		"refresh_token": "rt-u1-b",
		"expiration": "2099-06-01T00:00:00Z",
		"chat_id": "chat-9",
		"updated_at": 1700000100
	}`)
	_ = os.Remove(filepath.Join(authDir, "codearts-bad.json"))
	n, err = db.ImportCNAuthDir(authDir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("re-import = %d, want 1", n)
	}
	acc, err = db.Get("cn-u1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.AccessToken != "tok-u1-b" || acc.RefreshToken != "rt-u1-b" || acc.ChatID != "chat-9" {
		t.Fatalf("refresh import mismatch: %#v", acc)
	}
	countCN := 0
	for _, a := range db.List() {
		if a.Channel == ChannelCN {
			countCN++
		}
	}
	if countCN != 1 {
		t.Fatalf("cn accounts = %d, want 1 (dedup failed)", countCN)
	}
}

func TestImportCNAuthDirMissingDir(t *testing.T) {
	dataDir := t.TempDir()
	db, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Save()
	n, err := db.ImportCNAuthDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil || n != 0 {
		t.Fatalf("missing dir: n=%d err=%v, want 0/nil", n, err)
	}
}
