package kit

import "testing"

func TestObfRoundTrip(t *testing.T) {
	samples := []string{
		"https://example.com/path",
		"SDK-HMAC-SHA256",
		"application/json",
		"hello",
	}
	for _, s := range samples {
		enc := Enc(s)
		if X(enc) != s {
			t.Fatalf("b64 roundtrip fail %q -> %q -> %q", s, enc, X(enc))
		}
		hexEnc := EncHex(s)
		if Hex(hexEnc) != s {
			t.Fatalf("hex roundtrip fail %q", s)
		}
	}
}
