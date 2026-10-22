package huawei

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go-codearts/internal/kit"
)

// Hybrid region layout (intl CodeArts_Tui CLI):
// portal + models/quota @ ap-southeast-1; STS token + snap-manager ticket @ cn-north-4.
// Endpoint strings live in endpoints.go as kit.X ciphertext.

// oauthHTTP never honors HTTP(S)_PROXY from the environment.
// A stale Clash port (e.g. 127.0.0.1:10818) otherwise breaks STS token exchange.
var oauthHTTP = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

func oauthDo(ctx context.Context, req *http.Request) (*http.Response, error) {
	if c := ClientFromContext(ctx); c != nil {
		return c.Do(req)
	}
	return oauthHTTP.Do(req)
}

type DpopKeyPair struct {
	PrivateKeyJwk string `json:"private_key_jwk"`
	PublicKeyJwk  string `json:"public_key_jwk"`
}

type OAuthTokenResult struct {
	AccessKeyID     string       `json:"access_key_id"`
	SecretAccessKey string       `json:"secret_access_key"`
	SecurityToken   string       `json:"security_token"`
	Expiration      time.Time    `json:"expiration"`
	RefreshToken    string       `json:"refresh_token"`
	DpopKeyPair     *DpopKeyPair `json:"dpop_key_pair,omitempty"`
}

func leftPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

func randHexN(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hexdigits = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}

func GenerateDpopKeyPair() (*DpopKeyPair, *ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	pubBytes := elliptic.Marshal(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y)
	x := pubBytes[1:33]
	y := pubBytes[33:65]
	d := leftPad(priv.D.Bytes(), 32)
	pair := &DpopKeyPair{
		PrivateKeyJwk: base64.RawURLEncoding.EncodeToString(append(append(append([]byte{}, x...), y...), d...)),
		PublicKeyJwk:  base64.RawURLEncoding.EncodeToString(append(append([]byte{}, x...), y...)),
	}
	return pair, priv, nil
}

func DecodeDpopPrivateKey(pair *DpopKeyPair) (*ecdsa.PrivateKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(pair.PrivateKeyJwk)
	if err != nil {
		return nil, err
	}
	if len(raw) != 96 {
		return nil, fmt.Errorf("invalid private key length %d", len(raw))
	}
	x := new(big.Int).SetBytes(raw[0:32])
	y := new(big.Int).SetBytes(raw[32:64])
	d := new(big.Int).SetBytes(raw[64:96])
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, D: d}, nil
}

func publicJWK(priv *ecdsa.PrivateKey) map[string]string {
	pubBytes := elliptic.Marshal(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y)
	return map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(pubBytes[1:33]),
		"y":   base64.RawURLEncoding.EncodeToString(pubBytes[33:65]),
	}
}

func generateDpopJWT(priv *ecdsa.PrivateKey, method, htu string) (string, error) {
	header := map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": publicJWK(priv)}
	payload := map[string]any{
		"htm": method,
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": randHexN(16),
	}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(payload)
	h64 := base64.RawURLEncoding.EncodeToString(hb)
	p64 := base64.RawURLEncoding.EncodeToString(pb)
	input := h64 + "." + p64
	sum := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, priv, sum[:])
	if err != nil {
		return "", err
	}
	sig := append(leftPad(r.Bytes(), 32), leftPad(s.Bytes(), 32)...)
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func GeneratePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func ExchangeCodeForToken(ctx context.Context, code, verifier, redirectURI string) (*OAuthTokenResult, error) {
	pair, priv, err := GenerateDpopKeyPair()
	if err != nil {
		return nil, err
	}
	tokenURL := AuthTokenEndpoint + "/v1/oauth2/tokens"
	dpop, err := generateDpopJWT(priv, "POST", tokenURL)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("client_id", OAuthClientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("DPoP", dpop)
	resp, err := oauthDo(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("token exchange %d: %s", resp.StatusCode, kit.Snippet(string(raw), 300))
	}
	var data struct {
		Credentials struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SecurityToken   string `json:"security_token"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	exp, _ := time.Parse(time.RFC3339, data.Credentials.Expiration)
	return &OAuthTokenResult{
		AccessKeyID:     data.Credentials.AccessKeyID,
		SecretAccessKey: data.Credentials.SecretAccessKey,
		SecurityToken:   data.Credentials.SecurityToken,
		Expiration:      exp,
		RefreshToken:    data.RefreshToken,
		DpopKeyPair:     pair,
	}, nil
}

func ExchangeSecretForToken(ctx context.Context, ticketID, secret string) (*OAuthTokenResult, error) {
	u := fmt.Sprintf("%s/v1/login/ticket?%s", AuthTicketTokenEndpoint, url.Values{
		"ticket_id": {ticketID},
		"secret":    {secret},
	}.Encode())
	var lastErr error
	for i := 0; i < 10; i++ {
		time.Sleep(time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := oauthDo(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("ticket exchange %d: %s", resp.StatusCode, kit.Snippet(string(raw), 300))
			continue
		}
		var data struct {
			Credential struct {
				Access        string `json:"access"`
				Secret        string `json:"secret"`
				SecurityToken string `json:"securitytoken"`
				ExpiresAt     string `json:"expires_at"`
			} `json:"credential"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			lastErr = err
			continue
		}
		if data.Credential.Access == "" {
			lastErr = fmt.Errorf("empty credential")
			continue
		}
		exp, _ := time.Parse(time.RFC3339, data.Credential.ExpiresAt)
		return &OAuthTokenResult{
			AccessKeyID:     data.Credential.Access,
			SecretAccessKey: data.Credential.Secret,
			SecurityToken:   data.Credential.SecurityToken,
			Expiration:      exp,
			RefreshToken:    data.RefreshToken,
		}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("ticket exchange failed")
	}
	return nil, lastErr
}

func RefreshOAuthToken(ctx context.Context, refreshToken, accountID string, pair *DpopKeyPair) (*OAuthTokenResult, error) {
	priv, err := DecodeDpopPrivateKey(pair)
	if err != nil {
		return nil, err
	}
	tokenURL := AuthTokenEndpoint + "/v1/oauth2/tokens"
	dpop, err := generateDpopJWT(priv, "POST", tokenURL)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("client_id", OAuthClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("DPoP", dpop)
	if accountID != "" {
		req.Header.Set("x-agent-user-account", accountID)
	}
	resp, err := oauthDo(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("refresh %d: %s", resp.StatusCode, string(raw))
	}
	var data struct {
		Credentials struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SecurityToken   string `json:"security_token"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	exp, _ := time.Parse(time.RFC3339, data.Credentials.Expiration)
	rt := data.RefreshToken
	if rt == "" {
		rt = refreshToken
	}
	return &OAuthTokenResult{
		AccessKeyID:     data.Credentials.AccessKeyID,
		SecretAccessKey: data.Credentials.SecretAccessKey,
		SecurityToken:   data.Credentials.SecurityToken,
		Expiration:      exp,
		RefreshToken:    rt,
		DpopKeyPair:     pair,
	}, nil
}

func GetUserInfo(ctx context.Context, creds *OAuthTokenResult) (map[string]any, error) {
	u := AuthTicketTokenEndpoint + "/v1/current/user"
	headers, _, err := AKSKSign(creds.AccessKeyID, creds.SecretAccessKey, creds.SecurityToken, http.MethodGet, u, nil, map[string]string{
		"Accept": "application/json",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := oauthDo(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("userinfo %d: %s", resp.StatusCode, string(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
