package huawei

import (
	"crypto/rand"
	"encoding/hex"
)

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// fallback deterministic-ish
		for i := range b {
			b[i] = byte(i * 17)
		}
	}
	return hex.EncodeToString(b)
}
