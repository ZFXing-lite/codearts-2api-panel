package huawei

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// LocalCallback is a one-shot 127.0.0.1 listener that mirrors CodeArts CLI
// CallbackServer (path /oauth/callback, random port).
type LocalCallback struct {
	listener      net.Listener
	server        *http.Server
	port          int
	url           string
	expectedState string

	mu     sync.Mutex
	result *CallbackResult
	err    error
	done   chan struct{}
}

type CallbackResult struct {
	Code   string
	Secret string
	State  string
}

// StartLocalCallback listens on 127.0.0.1:0 and serves /oauth/callback.
func StartLocalCallback(expectedState ...string) (*LocalCallback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	state := ""
	if len(expectedState) > 0 {
		state = expectedState[0]
	}
	cb := &LocalCallback{
		listener:      ln,
		port:          port,
		url:           fmt.Sprintf("http://127.0.0.1:%d%s", port, OAuthCallbackPath),
		expectedState: state,
		done:          make(chan struct{}),
	}
	mux := http.NewServeMux()
	// Official path + catch-all: some portal builds hit / or /callback.
	mux.HandleFunc(OAuthCallbackPath, cb.handle)
	mux.HandleFunc("/callback", cb.handle)
	mux.HandleFunc("/", cb.handle)
	cb.server = &http.Server{Handler: mux}
	go func() { _ = cb.server.Serve(ln) }()
	log.Printf("oauth local callback listening on %s", cb.url)
	return cb, nil
}

func (c *LocalCallback) Port() int       { return c.port }
func (c *LocalCallback) URL() string     { return c.url }
func (c *LocalCallback) PortStr() string { return fmt.Sprintf("%d", c.port) }

func (c *LocalCallback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	secret := q.Get("secret")
	state := q.Get("state")
	if state == "" {
		// CodeArts portal variants echo the original ticket_id instead of state.
		state = q.Get("ticket_id")
	}
	redirect := q.Get("redirect")
	errMsg := q.Get("error")

	log.Printf("oauth callback hit path=%s code=%v secret=%v redirect=%v err=%q",
		r.URL.Path, code != "", secret != "", redirect != "", errMsg)

	// Ignore probes without oauth params — do NOT finish the waiter.
	if errMsg == "" && code == "" && secret == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>waiting</title>
<body style="font-family:system-ui;padding:40px;background:#0b0f14;color:#d7dde5">
<h1>等待授权回调</h1><p>本地回调已就绪，请在授权页点击确认。</p></body>`)
		return
	}

	if errMsg != "" {
		desc := q.Get("error_description")
		if desc == "" {
			desc = errMsg
		}
		c.finish(nil, fmt.Errorf("oauth error: %s", desc))
		if redirect != "" {
			http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
			return
		}
		writeCallbackHTML(w, false, desc)
		return
	}

	if c.expectedState != "" && state != c.expectedState {
		c.finish(nil, fmt.Errorf("oauth state mismatch"))
		writeCallbackHTML(w, false, "OAuth state mismatch")
		return
	}
	c.finish(&CallbackResult{Code: code, Secret: secret, State: state}, nil)

	// Official CLI: if portal sent redirect=, 307 back to portal success page.
	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	writeCallbackHTML(w, true, "")
}

func (c *LocalCallback) finish(res *CallbackResult, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return
	default:
		c.result = res
		c.err = err
		close(c.done)
	}
}

// Wait blocks until the browser hits the callback or ctx/timeout fires.
func (c *LocalCallback) Wait(ctx context.Context) (*CallbackResult, error) {
	select {
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.result, c.err
	}
}

func (c *LocalCallback) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if c.server != nil {
		_ = c.server.Shutdown(ctx)
	}
	if c.listener != nil {
		_ = c.listener.Close()
	}
	return nil
}

func writeCallbackHTML(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	title := "授权成功"
	body := "授权已完成，窗口即将自动关闭。"
	script := `<script>
try{window.close();}catch(e){}
setTimeout(function(){try{window.close();}catch(e){}},400);
setTimeout(function(){
  document.getElementById('hint').style.display='block';
},1200);
</script>`
	if !ok {
		title = "授权失败"
		body = msg
		if body == "" {
			body = "授权失败，请重试。"
		}
		script = ""
	}
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head>
<body style="font-family:system-ui;padding:40px;background:#0b0f14;color:#d7dde5">
<h1>%s</h1><p>%s</p>
<p id="hint" style="display:none;opacity:.7">若窗口未自动关闭，请手动关掉此页并返回 Manager。</p>
%s</body></html>`, title, title, body, script)
}

// BuildAuthorizeURL builds the official CodeArts CLI authorize URL.
// Only: locale, client_id, port, code_challenge, code_challenge_method, ticket_id.
// Do NOT put redirect_uri / prompt here — that breaks the portal (authorize click does nothing).
func BuildAuthorizeURL(localPort, ticketID, codeChallenge string) string {
	params := url.Values{}
	params.Set("locale", "zh-cn")
	params.Set("client_id", OAuthClientID)
	params.Set("port", localPort)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "SHA-256")
	params.Set("ticket_id", ticketID)
	return fmt.Sprintf("%s/authorize?%s", strings.TrimRight(AuthPortalEndpoint, "/"), params.Encode())
}
