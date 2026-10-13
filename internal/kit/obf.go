package kit

import (
	"encoding/base64"
	"encoding/hex"
)

var (
	obfK1 = []byte{0x5A, 0x3C, 0x2E, 0x77, 0x11, 0x9F, 0x40, 0xE8}
	obfK2 = []byte{0xB7, 0x14, 0x65, 0xC3, 0x08, 0x2D, 0xFE, 0x91}
)

func obfPass(b []byte) {
	for i := range b {
		b[i] ^= obfK1[i%len(obfK1)]
	}
}

func obfPass2(b []byte) {
	for i := range b {
		b[i] = b[i]<<3 | b[i]>>5
		b[i] ^= obfK2[i%len(obfK2)]
	}
}

func obfPass2Inv(b []byte) {
	for i := range b {
		b[i] ^= obfK2[i%len(obfK2)]
		b[i] = b[i]>>3 | b[i]<<5
	}
}

func obfDecode(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	obfPass2(blob)
	obfPass(blob)
	return string(blob)
}

func isHexLike(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return len(s) > 0 && len(s)%2 == 0
}

// X 双层混淆解码: 自动识别 base16/hex 或 base64 编码
func X(s string) string {
	if isHexLike(s) {
		b, err := hex.DecodeString(s)
		if err == nil && len(b) > 0 {
			return obfDecode(b)
		}
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return ""
	}
	return obfDecode(b)
}

// Hex 显式 hex 密文解码
func Hex(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil {
		return ""
	}
	return obfDecode(b)
}

// Enc 明文 → base64 密文（开发期生成用）
func Enc(plain string) string {
	b := []byte(plain)
	obfPass(b)
	obfPass2Inv(b)
	return base64.StdEncoding.EncodeToString(b)
}

// EncHex 明文 → hex 密文
func EncHex(plain string) string {
	b := []byte(plain)
	obfPass(b)
	obfPass2Inv(b)
	return hex.EncodeToString(b)
}
