// cnlogin：国内码道（华为云 CodeArts）OAuth2/PKCE 登录工具。
//
// 移植自 codearts2api-cn/cmd/login，适配 go-codearts 内部包：
//   - 上游客户端：internal/cnup（PKCE/authorize/ExchangeCode/PollTicket/DPoP）
//   - 落盘格式：internal/cnauth（CLI / 网关 / 面板共用同一 schema）
//   - 可选入库：-import 调 internal/store 的 ImportCNAuthDir
//
// 用法：
//
//	cnlogin [-auth-dir DIR] [-client-id ID] [-import] [-data-dir DIR]
//	cnlogin -print-only [-port 8910]    # 服务器/容器：打印链接，粘贴回调完成
//
// 关于回调：portal 的回调地址是 http://127.0.0.1:{port}/oauth/callback，
// 由授权链接的 port 参数决定，无法改成域名。所以：
//   - 有浏览器且与服务同机：默认模式起本地回调服务，点完即完成；
//   - 纯服务器：-print-only 打印链接，用任意机器浏览器打开。登录后浏览器会
//     跳到 127.0.0.1(那台机器)打不开——这是正常的，把地址栏整段 URL 粘回
//     终端即可（含 code 直接换 token；含 secret 则用它轮询 ticket）。
//
// 早期版本 -print-only 只是空转轮询本地 secret，而上游不认本地 secret，
// 于是必然 5 分钟超时——现在的粘贴通道才是服务器场景的正解。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go-codearts/internal/cnauth"
	"go-codearts/internal/cnup"
	"go-codearts/internal/config"
	"go-codearts/internal/store"
)

func main() {
	cfg := config.Get()
	authDir := flag.String("auth-dir", cfg.CNAuthDir, "auth output dir (env CN_AUTH_DIR)")
	dataDir := flag.String("data-dir", cfg.DataDir, "gateway data dir for -import (env DATA_DIR)")
	printOnly := flag.Bool("print-only", false, "server mode: print login link, paste the callback URL back")
	doImport := flag.Bool("import", false, "import auths dir into gateway store after login")
	clientID := flag.String("client-id", "", "OAuth client id (default: official)")
	portFlag := flag.Int("port", 0, "callback port (print-only default 8910)")
	flag.Parse()

	lcfg := cnup.DefaultLoginConfig()
	if *clientID != "" {
		lcfg.ClientID = *clientID
	}
	client := cnup.New(60 * time.Second)
	ctx := context.Background()

	// 回调端口必须与授权链接、redirect_uri 三方一致，否则 code 换 token 会被拒。
	callbackPort := *portFlag
	if callbackPort == 0 {
		if *printOnly {
			callbackPort = 8910
		} else {
			callbackPort = 0 // 下面按实际监听端口填
		}
	}

	// 1. 本地回调服务（print-only 也起，能通就用它，通不了走粘贴）
	var ln net.Listener
	var err error
	codeCh := make(chan string, 1)
	cb := &callbackState{}
	if !*printOnly {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("loopback listen: %v", err)
		}
		callbackPort = ln.Addr().(*net.TCPAddr).Port
	} else if callbackPort > 0 {
		// print-only：尝试占用固定端口。被占用不影响主流程（走粘贴）。
		if l, lerr := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(callbackPort)); lerr == nil {
			ln = l
		}
	}
	if ln != nil {
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
	// 不再本地编 secret：ticket 轮询只认 portal 下发的那个（回环回调或粘贴）。
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
	if *printOnly {
		fmt.Printf("print-only 模式：用任意浏览器打开上面的链接登录。\n")
		fmt.Printf("登录后浏览器会跳到 http://127.0.0.1:%d%s?... 这一跳打不开是正常的，\n", callbackPort, lcfg.RedirectPath)
		fmt.Println("把地址栏里那条完整 URL 整段粘贴到下面并回车（含 code 或 secret 均可）。")
		fmt.Println("提示：URL 太长可只贴问号后面的部分。")
	} else {
		_ = openBrowser(loginURL)
		fmt.Printf("已在浏览器打开；若未打开请手动访问。回调端口 %d。\n", callbackPort)
	}

	// 3. 三通道：本地回调 code / 粘贴 URL / portal ticket 轮询
	var tok *cnup.TokenResponse
	pasteCh := make(chan *url.URL, 1)
	if *printOnly {
		go readStdinURL(pasteCh)
	}
	ticker := time.NewTicker(2 * time.Second)
	timer := time.NewTimer(10 * time.Minute)
	defer ticker.Stop()
	defer timer.Stop()

	fail := func(msg string) {
		log.Fatalf("%s", msg)
	}
loginLoop:
	for {
		select {
		case <-timer.C:
			fail("登录超时（10 分钟）。重新运行本工具，或改用面板「国内码道 → 新增账号」。")
		case code := <-codeCh:
			tok, err = client.ExchangeCode(ctx, lcfg, code, verifier, callbackPort, dpopJWK)
			if err != nil {
				fail("exchange code: " + err.Error())
			}
			break loginLoop
		case u := <-pasteCh:
			q := u.Query()
			if code := q.Get("code"); code != "" {
				tok, err = client.ExchangeCode(ctx, lcfg, code, verifier, callbackPort, dpopJWK)
				if err != nil {
					fmt.Printf("用该 code 换 token 失败：%v\n（可能是 code 已用过或端口不匹配，请重新粘贴新的回调地址）\n", err)
					continue
				}
				break loginLoop
			}
			if sec := q.Get("secret"); sec != "" {
				// portal 下发的 secret 才是 ticket 轮询认可的凭据。
				cb.setSecret(sec)
				fmt.Println("已接收 portal secret，正在等待上游放行…")
				if next := q.Get("redirect"); next != "" {
					fmt.Println("如果浏览器还停在回调页，可继续打开：\n  " + next)
				}
				continue
			}
			fmt.Println("这段 URL 里既没有 code 也没有 secret，请确认粘的是登录后浏览器跳转的那一条。")
		case <-ticker.C:
			sec := cb.pollSecret("")
			if sec == "" {
				continue // 没有 portal secret 时轮询只会白撞
			}
			t, terr := client.PollTicket(ctx, lcfg, ticketID, sec)
			if terr == nil && t != nil && t.UserName != "" && t.Credentials.SecurityToken != "" {
				tok = t
				break loginLoop
			}
		}
	}

	// 4. 落盘
	cred := tok.Credentials
	rec := &cnauth.File{
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
	path, err := cnauth.Save(*authDir, rec)
	if err != nil {
		log.Fatalf("save auth: %v", err)
	}
	fmt.Printf("\n登录成功：user_id=%s name=%s\n", tok.UserID, tok.UserName)
	fmt.Printf("凭证已保存：%s\n", path)
	if cred.Expiration != "" {
		fmt.Printf("STS 有效期至：%s（到期前网关会自动续期）\n", cred.Expiration)
	}
	if tok.RefreshToken == "" {
		fmt.Println("警告：本次响应没带 refresh_token，到期后需重新登录。")
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

// readStdinURL 逐行读粘贴的回调地址。允许只贴问号后的 query 部分。
func readStdinURL(out chan<- *url.URL) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "?") {
			line = "http://127.0.0.1/oauth/callback" + line
		}
		u, err := url.Parse(line)
		if err != nil {
			fmt.Println("这行不是合法 URL，请重新粘贴。")
			continue
		}
		select {
		case out <- u:
		default:
		}
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
			body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			vals, _ := url.ParseQuery(string(body))
			code = vals.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if code == "" {
			// 只收到 secret 不算失败：主循环会继续用 ticket 轮询。
			_, _ = w.Write([]byte("<h3>已接收授权回调，等待上游放行…</h3>"))
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
