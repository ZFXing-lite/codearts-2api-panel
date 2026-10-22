package auth

import "testing"

// 登出必须让服务端立即失效：cookie 是 HttpOnly，前端删不掉，
// 只清 cookie 的话 token 在 24h TTL 内始终可用。
func TestRevokeInvalidatesSession(t *testing.T) {
	m, err := New("admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := m.Login("pw")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Validate(tok) {
		t.Fatal("fresh token should be valid")
	}
	m.Revoke(tok)
	if m.Validate(tok) {
		t.Fatal("revoked token still valid")
	}
	// 撤销只影响这一个会话，别的会话照常。
	other, _ := m.Login("pw")
	if !m.Validate(other) {
		t.Fatal("revoke must not affect other sessions")
	}
}

// 伪造签名不能被塞进撤销表（否则等于给了外部一个写入口）。
func TestRevokeRejectsForgedToken(t *testing.T) {
	m, _ := New("admin", "pw")
	tok, _ := m.Login("pw")
	forged := tok + "x"
	m.Revoke(forged)
	// 真实会话必须不受影响：伪造 token 的签名校验不通过，不该进撤销表。
	if !m.Validate(tok) {
		t.Fatal("forged revoke touched a real session")
	}
	if len(m.revoked) != 0 {
		t.Fatalf("forged token entered revocation table: %v", m.revoked)
	}
}
