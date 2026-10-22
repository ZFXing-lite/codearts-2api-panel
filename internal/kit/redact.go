package kit

// 上游响应体经常被整段回显进错误信息：ApiError.Message / UpstreamError.Body
// 最终会流进 store.LastError、面板和日志。token 与 AK/SK 就在那些报文里，
// 所以任何"截取一段响应体"的地方都必须先脱敏、再按字符边界截断。
//
// 国际版（internal/huawei）与国内码道（internal/cnup）共用这两步，
// 避免两边各写一份、各自漏字段。

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var sensitiveFieldRe = regexp.MustCompile(
	`(?i)"(access_token|refresh_token|security_token|secret_access_key|access_key_id|` +
		`code_verifier|dpop_private_key|password|authorization|x-auth-token|client_secret|ak|sk)"` +
		`\s*:\s*("(?:[^"\\]|\\.)*"|[^,}\s]+)`)

// RedactSensitive 把 JSON 报文里的凭证字段值抹成 ***。
// 非 JSON 文本原样返回（正则匹配不到就什么都不改）。
func RedactSensitive(s string) string {
	if s == "" {
		return s
	}
	return sensitiveFieldRe.ReplaceAllString(s, `"$1":"***"`)
}

// TruncateUTF8 按字符边界截断，超长时补省略号。
// 直接切字节会把中文/emoji 切成半个字符，面板与日志里就是乱码。
func TruncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		cut = n
	}
	return s[:cut] + "…"
}

// Snippet 是"安全片段"：先脱敏、再截断。用于错误信息里附带上游原文。
func Snippet(s string, n int) string {
	return TruncateUTF8(strings.TrimSpace(RedactSensitive(s)), n)
}
