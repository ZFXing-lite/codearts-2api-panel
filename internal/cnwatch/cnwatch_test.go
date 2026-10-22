package cnwatch

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"go-codearts/internal/cnup"
	"go-codearts/internal/config"
	"go-codearts/internal/store"
)

// fakeClient 记录调用并可编程返回错误。
type fakeClient struct {
	refreshErr   error
	refreshRes   *cnup.TokenResponse
	refreshCalls int
	checkinCalls int
	claimCalls   int
	chatCalls    int
	chatErr      error
	modelCalls   int
	modelRes     []cnup.ModelInfo
	modelErr     error
}

func (f *fakeClient) FetchModels(*cnup.Account) ([]cnup.ModelInfo, error) {
	f.modelCalls++
	return f.modelRes, f.modelErr
}

func (f *fakeClient) RefreshToken(context.Context, cnup.LoginConfig, string, string, cnup.DPoPPrivateJWK) (*cnup.TokenResponse, error) {
	f.refreshCalls++
	return f.refreshRes, f.refreshErr
}
func (f *fakeClient) Checkin(context.Context, cnup.SignCredential) (cnup.CheckinResult, error) {
	f.checkinCalls++
	return cnup.CheckinResult{OK: true, Message: "ok"}, nil
}
func (f *fakeClient) ClaimBenefit(cnup.SignCredential) error {
	f.claimCalls++
	return nil
}
func (f *fakeClient) ChatStream(context.Context, string, []cnup.ChatMessage, string, cnup.SignCredential, string, string, bool) (io.ReadCloser, error) {
	f.chatCalls++
	if f.chatErr != nil {
		return nil, f.chatErr
	}
	return io.NopCloser(strings.NewReader("data: {}\n\n")), nil
}

func testConfig() config.Config {
	return config.Config{
		CNEnabled:           true,
		CNDefaultModel:      "snap-chat",
		CNWatchPoll:         time.Minute,
		CNRefreshSkew:       30 * time.Minute,
		CNKeepaliveWindow:   10 * time.Minute,
		CNKeepaliveInterval: 15 * time.Minute,
		CNCreditsAutoClaim:  true,
		CNBenefitAutoClaim:  true,
	}
}

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return db
}

func cnAccount(db *store.DB, id string, expires time.Time) *store.Account {
	acc := &store.Account{
		ID:           id,
		Email:        id + "@example.com",
		Channel:      store.ChannelCN,
		UserID:       id,
		AccessToken:  "tok-old",
		RefreshToken: "rt-old",
		TokenExpires: expires,
		Opened:       true,
		Credentials: &store.Credentials{
			AccessKeyID:     "ak-old",
			SecretAccessKey: "sk-old",
			SecurityToken:   "tok-old",
			Expiration:      expires,
			AuthMode:        "oauth_sts",
		},
	}
	if err := db.Upsert(acc); err != nil {
		panic(err)
	}
	return acc
}

// 目录不存在时 Import 不炸（store 层已保证返回 (0, nil)）。
func TestImportMissingDir(t *testing.T) {
	db := openDB(t)
	n, err := db.ImportCNAuthDir(t.TempDir() + "/no-such-dir")
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 imports, got %d", n)
	}
}

// refresh_token 终态失效：账号打 cn_needs_relogin 标记，凭证不被改写，
// 同轮后续也跳过 checkin/chat。
func TestRefreshExpiredMarksRelogin(t *testing.T) {
	db := openDB(t)
	fc := &fakeClient{refreshErr: &cnup.RefreshTokenExpiredError{Status: 400, Message: "invalid_grant"}}
	w := NewWithClient(db, testConfig(), fc)

	cnAccount(db, "cn-1", time.Now().Add(5*time.Minute)) // 已临期
	w.scanOnce(context.Background())

	if fc.refreshCalls != 1 {
		t.Fatalf("refresh should be called once, got %d", fc.refreshCalls)
	}
	got, err := db.SnapshotAccount("cn-1")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got.Metadata["cn_needs_relogin"] != "true" {
		t.Fatalf("expected cn_needs_relogin=true, metadata=%v", got.Metadata)
	}
	if got.AccessToken != "tok-old" || got.RefreshToken != "rt-old" {
		t.Fatalf("tokens must stay untouched on terminal failure: %+v", got)
	}
	// 第二轮：已打标账号不再打上游。
	w.scanOnce(context.Background())
	if fc.refreshCalls != 1 {
		t.Fatalf("marked account must be skipped, refresh calls=%d", fc.refreshCalls)
	}
	if fc.checkinCalls != 0 || fc.chatCalls != 0 {
		t.Fatalf("expired account should not checkin/ping, got checkin=%d chat=%d", fc.checkinCalls, fc.chatCalls)
	}
}

// 正常刷新：AccessToken/RefreshToken/TokenExpires/AK/SK 写回并持久化。
func TestRefreshSuccessWritesBack(t *testing.T) {
	db := openDB(t)
	fc := &fakeClient{refreshRes: &cnup.TokenResponse{
		UserID:       "cn-2",
		RefreshToken: "rt-new",
		Credentials: cnup.Credentials{
			AccessKeyID:     "ak-new",
			SecretAccessKey: "sk-new",
			SecurityToken:   "tok-new",
			Expiration:      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		},
	}}
	w := NewWithClient(db, testConfig(), fc)

	cnAccount(db, "cn-2", time.Now().Add(5*time.Minute))
	w.scanOnce(context.Background())

	got, err := db.SnapshotAccount("cn-2")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got.AccessToken != "tok-new" || got.RefreshToken != "rt-new" {
		t.Fatalf("tokens not updated: %+v", got)
	}
	if got.Credentials == nil || got.Credentials.AccessKeyID != "ak-new" || got.Credentials.SecretAccessKey != "sk-new" {
		t.Fatalf("ak/sk not updated: %+v", got.Credentials)
	}
	if got.TokenExpires.Before(time.Now().Add(time.Hour)) {
		t.Fatalf("expires not updated: %v", got.TokenExpires)
	}
	if got.Metadata["cn_needs_relogin"] == "true" {
		t.Fatal("relogin flag must not be set on success")
	}
	if fc.checkinCalls != 1 || fc.claimCalls != 1 {
		t.Fatalf("checkin/benefit should run once, got %d/%d", fc.checkinCalls, fc.claimCalls)
	}
}

// 保活：沉寂账号发一次 ping 并记录时间戳；时间窗内不重复发，失败不致命。
func TestKeepalive(t *testing.T) {
	db := openDB(t)
	fc := &fakeClient{refreshRes: nil, refreshErr: nil}
	cfg := testConfig()
	w := NewWithClient(db, cfg, fc)

	cnAccount(db, "cn-3", time.Now().Add(24*time.Hour)) // 未临期，不触发刷新
	w.scanOnce(context.Background())
	if fc.refreshCalls != 0 {
		t.Fatalf("fresh token must not refresh, calls=%d", fc.refreshCalls)
	}
	if fc.chatCalls != 1 {
		t.Fatalf("inactive account should be pinged once, got %d", fc.chatCalls)
	}
	got, _ := db.SnapshotAccount("cn-3")
	if got.Metadata["cn_last_keepalive"] == "" {
		t.Fatal("cn_last_keepalive not recorded")
	}

	// 间隔未到：第二轮不重复发。
	w.scanOnce(context.Background())
	if fc.chatCalls != 1 {
		t.Fatalf("ping within interval must be suppressed, got %d", fc.chatCalls)
	}

	// 心跳失败不致命、不写 needs_relogin。
	fc.chatErr = io.ErrClosedPipe
	w = NewWithClient(db, cfg, fc)
	w.now = func() time.Time { return time.Now().Add(30 * time.Minute) }
	w.scanOnce(context.Background())
	if fc.chatCalls != 2 {
		t.Fatalf("stale heartbeat should re-ping, got %d", fc.chatCalls)
	}
	got, _ = db.SnapshotAccount("cn-3")
	if got.Metadata["cn_needs_relogin"] == "true" {
		t.Fatal("keepalive failure must not mark relogin")
	}
}
