package huawei

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func sdkSigningAlgorithm() string { return SDKHMACSHA256 }

func hmacSHA256Hex(secret, data []byte) string {
	h := hmac.New(sha256.New, secret)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// percentEncode mimics the AKSKSigner2.urlEncode implementation extracted from
// the CodeArts CLI binary.  Non-ASCII and reserved characters are percent-encoded
// uppercase; unreserved chars A-Z a-z 0-9 - _ . ~ are left as-is.
// Processes by rune (not byte) to correctly handle multi-byte UTF-8.
func percentEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == '~' {
			b.WriteRune(r)
		} else {
			for _, c := range []byte(string(r)) {
				b.WriteByte('%')
				b.WriteByte(hexDigits[c>>4])
				b.WriteByte(hexDigits[c&0x0f])
			}
		}
	}
	return b.String()
}

func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	parts := strings.Split(path, "/")
	enc := make([]string, 0, len(parts))
	for _, p := range parts {
		enc = append(enc, percentEncode(p))
	}
	out := strings.Join(enc, "/")
	// Huawei's CLI signer canonicalizes every URI with a trailing slash.
	// Omitting it produces APIG.0301 signature failures.
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

func canonicalQueryString(query string) string {
	if query == "" {
		return ""
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return ""
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := []string{}
	for _, k := range keys {
		vals := values[k]
		sort.Strings(vals)
		for _, v := range vals {
			pairs = append(pairs, percentEncode(k)+"="+percentEncode(v))
		}
	}
	return strings.Join(pairs, "&")
}

func canonicalHeaders(headers map[string]string) (string, string) {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
	})
	var canonical strings.Builder
	signed := make([]string, 0, len(keys))
	for _, k := range keys {
		lower := strings.ToLower(k)
		signed = append(signed, lower)
		canonical.WriteString(lower + ":" + headers[k] + "\n")
	}
	return canonical.String(), strings.Join(signed, ";")
}

func buildCanonicalRequest(method, uri, query, canonicalHeaders, signedHeaders, payloadHash string) string {
	return strings.Join([]string{method, uri, query, canonicalHeaders, signedHeaders, payloadHash}, "\n")
}

func getStringToSign(date, canonicalRequestHash string) string {
	return strings.Join([]string{sdkSigningAlgorithm(), date, canonicalRequestHash}, "\n")
}

// AKSKSign signs a Huawei Cloud request the same way as the CodeArts CLI.
// body is the exact request payload; extraHeaders may contain X-Security-Token.
func AKSKSign(ak, sk, securityToken, method, rawURL string, body []byte, extraHeaders map[string]string) (map[string]string, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", err
	}
	date := time.Now().UTC().Format("20060102T150405Z")
	headers := map[string]string{}
	if securityToken != "" {
		headers["X-Security-Token"] = securityToken
	}
	for k, v := range extraHeaders {
		headers[k] = v
	}
	// The CLI signer deletes content-type unless multipart; keep behavior.
	if ct := headers["Content-Type"]; strings.Contains(ct, "multipart/form-data") {
		// keep
	} else {
		delete(headers, "Content-Type")
	}
	headers["X-Sdk-Date"] = date
	headers["Host"] = u.Host

	payloadHash := emptyBodySHA256
	if len(body) > 0 {
		payloadHash = sha256Hex(body)
	}
	canonicalHeadersStr, signedHeaders := canonicalHeaders(headers)
	canonicalReq := buildCanonicalRequest(method, canonicalURI(u.Path), canonicalQueryString(u.RawQuery), canonicalHeadersStr, signedHeaders, payloadHash)
	canonicalReqHash := sha256Hex([]byte(canonicalReq))
	stringToSign := getStringToSign(date, canonicalReqHash)
	signature := hmacSHA256Hex([]byte(sk), []byte(stringToSign))
	headers["Authorization"] = fmt.Sprintf("%s Access=%s, SignedHeaders=%s, Signature=%s", sdkSigningAlgorithm(), ak, signedHeaders, signature)
	return headers, date, nil
}
