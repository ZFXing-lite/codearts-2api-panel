// cnlogin：国内码道（华为云 CodeArts）OAuth2/PKCE 登录工具。
//
// 移植自 codearts2api-cn/cmd/login，适配 go-codearts 内部包：
//   - 上游客户端：internal/cnup（PKCE/authorize/ExchangeCode/PollTicket/DPoP）
//   - 落盘格式：对齐 codearts2api-cn/internal/auth 的 schema
//   - 可选入库：-import 调 internal/store 的 ImportCNAuthDir
//
// 用法：
//
//	cnlogin [-print-only] [-auth-dir DIR] [-client-id ID] [-import] [-data-dir DIR]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"go-codearts/internal/cnup"
	"go-codearts/internal/config"
	"go-codearts/internal/store"
)

// authFile 是 auths/codearts-{user_id}.json 的落盘 schema，
// 字段名与 codearts2api-cn/internal/auth/auth.go 一致，导入方也按此解析。
type authFile struct {
	UserID          string            `json:"user_id"`
	UserName        string            `json:"user_name"`
	DomainID        string            `json:"domain_id"`
	CloudDragonTok  string            `json:"cloud_dragon_token"` // = STS security_token
	AccessKeyID     string            `json:"access_key_id"`
	SecretAccessKey string            `json:"secret_access_key"`
	Expiration      string            `json:"expiration"` // RFC3339
	RefreshToken    string            `json:"refresh_token"`
	CodeVerifier    string            `json:"code_verifier"` // PKCE verifier，refresh 需要
	UpdatedAt       int64             `json:"updated_at"`
	ClientID        string            `json:"client_id,omitempty"`
	DPoPPrivateKey  map[string]string `json:"dpop_private_key,omitempty"`
}

// fileName 返回该账号的凭证文件名。
func fileName(userID string) string { return "codearts-" + userID + ".json" }

// saveAuth 写盘：临时文件 + rename，避免半写文件被导入方读到。
func saveAuth(dir string, a *authFile) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fileName(a.UserID))
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

func main() {
	cfg := config.Get()
	authDir := flag.String("auth-dir", cfg.CNAuthDir, "auth output dir (env CN_AUTH_DIR)")
	dataDir := flag.String("data-dir", cfg.DataDir, "gateway data dir for -import (env DATA_DIR)")
	printOnly := flag.Bool("print-only", false, "server mode: print login link and poll (no local browser/callback)")
	doImport := flag.Bool("import", false, "import auths dir into gateway store after login")
	clientID := flag.String("client-id", "", "OAuth client id (default: official)")
	flag.Parse()

	lcfg := cnup.DefaultLoginConfig()
	if *clientID != "" {
		lcfg.ClientID = *clientID
	}
	client := cnup.New(60 * time.Second)
	ctx := context.Background()

	// 1. 本地回调服务（print-only 模式跳过：服务器没有浏览器可回跳）
	var ln net.Listener
	var err error
	callbackPort := 0
	codeCh := make(chan string, 1)
	cb := &callbackState{}
	if !*printOnly {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("loopback listen: %v", err)
		}
		callbackPort = ln.Addr().(*net.TCPAddr).Port
		go serveCallback(ln, lcfg.RedirectPath, cb, codeCh)
		defer ln.Close()
	}

	// 2. ticket/secret + PKCE + DPoP
	//
	// DPoP 私钥必须与 refresh_token 一起持久化：refresh_token 与签发时的
	// DPoP 公钥绑定，换密钥刷新会被 STS 拒（InvalidDPoPHeader）。
	ticketID, err := cnup.RandomHex(16)
	if err != nil {
		log.Fatalf("gen ticket: %v", err)
	}
	secret, err := cnup.RandomHex(16)
	if err != nil {
		log.Fatalf("gen secret: %v", err)
	}
	verifier, challenge, err := cnup.PKCE()
	if err != nil {
		log.Fatalf("gen pkce: %v", err)
	}
	dpopJWK, err := cnup.NewDPoPPrivateJWK()
	if err != nil {
		log.Fatalf("gen dpop: %v", err)
	}

	loginURL := client.BuildAuthorizeURL(lcfg, ticketID, challenge, "SHA-256", callbackPort)
	fmt.Println("============================================================")
	fmt.Println("  CodeArts 国内码道登录（华为云账号）")
	fmt.Println("============================================================")
	fmt.Println("登录链接：")
	fmt.Println("  " + loginURL)
	fmt.Println("")
	if !*printOnly {
		_ = openBrowser(loginURL)
	} else {
		fmt.Println("print-only 模式：无本地浏览器，登录成功后本工具轮询 ticket 取回凭证。")
	}

	// 3. 双通道：回调 code vs ticket 轮询
	var tok *cnup.TokenResponse
	ticker := time.NewTicker(2 * time.Second)
	timer := time.NewTimer(5 * time.Minute)
	defer ticker.Stop()
	defer timer.Stop()
loginLoop:
	for {
		select {
		case <-timer.C:
			log.Fatalf("登录超时（5 分钟）")
		case code := <-codeCh:
			tok, err = client.ExchangeCode(ctx, lcfg, code, verifier, callbackPort, dpopJWK)
			if err != nil {
				log.Fatalf("exchange code: %v", err)
			}
			break loginLoop
		case <-ticker.C:
			t, terr := client.PollTicket(ctx, lcfg, ticketID, cb.pollSecret(secret))
			if terr == nil && t != nil && t.UserName != "" {
				tok = t
				break loginLoop
			}
		}
	}

	// 4. 落盘
	cred := tok.Credentials
	a := &authFile{
		UserID:          tok.UserID,
		UserName:        tok.UserName,
		DomainID:        tok.DomainID,
		CloudDragonTok:  cred.SecurityToken,
		AccessKeyID:     cred.AccessKeyID,
		SecretAccessKey: cred.SecretAccessKey,
		Expiration:      cred.Expiration,
		RefreshToken:    tok.RefreshToken,
		CodeVerifier:    verifier,
		UpdatedAt:       time.Now().Unix(),
		ClientID:        lcfg.ClientID,
		DPoPPrivateKey:  map[string]string(dpopJWK),
	}
	path, err := saveAuth(*authDir, a)
	if err != nil {
		log.Fatalf("save auth: %v", err)
	}
	fmt.Printf("\n登录成功：user_id=%s name=%s\n", tok.UserID, tok.UserName)
	fmt.Printf("凭证已保存：%s\n", path)
	if cred.Expiration != "" {
		fmt.Printf("STS 有效期至：%s\n", cred.Expiration)
	}

	// 5. 可选入库
	if *doImport {
		db, err := store.Open(*dataDir)
		if err != nil {
			log.Fatalf("open store %s: %v", *dataDir, err)
		}
		n, err := db.ImportCNAuthDir(*authDir)
		if err != nil {
			log.Fatalf("import auths: %v", err)
		}
		fmt.Printf("已入库 %d 个账号（data dir: %s）\n", n, *dataDir)
	}
}

// callbackState 保存 portal 回调里下发的 secret。
//
// portal 首次回调只带 secret + redirect（不含 code），而轮询 /v1/login/ticket
// 必须用 portal 下发的 secret（本地生成的那份上游不认），两边共用回调里的值。
type callbackState struct {
	mu     sync.Mutex
	secret string
}

func (s *callbackState) setSecret(v string) {
	s.mu.Lock()
	s.secret = v
	s.mu.Unlock()
}

func (s *callbackState) pollSecret(fallback string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secret != "" {
		return s.secret
	}
	return fallback
}

// serveCallback 处理本地回调（GET query 或 POST body 传 code）。
//
// portal 会回调两次：第一次带 secret + redirect，要求 307 跳回 redirect；
// 这一次报错会中断链路，带 code 的第二次回调就永远不会到达。
func serveCallback(ln net.Listener, redirectPath string, st *callbackState, ch chan<- string) {
	mux := http.NewServeMux()
	mux.HandleFunc(redirectPath, func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if secret := r.URL.Query().Get("secret"); secret != "" {
			st.setSecret(secret)
		}
		if code == "" {
			if redirect := r.URL.Query().Get("redirect"); redirect != "" {
				http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
				return
			}
			// 兼容 POST body 传 code
			body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			vals, _ := url.ParseQuery(string(body))
			code = vals.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<h3>登录失败：缺少 code</h3>"))
			return
		}
		_, _ = w.Write([]byte("<h3>登录成功，可关闭此页面。</h3>"))
		select {
		case ch <- code:
		default:
		}
	})
	_ = http.Serve(ln, mux)
}

func openBrowser(rawurl string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawurl)
	case "darwin":
		cmd = exec.Command("open", rawurl)
	default:
		cmd = exec.Command("xdg-open", rawurl)
	}
	return cmd.Start()
}
