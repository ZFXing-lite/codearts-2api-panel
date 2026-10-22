package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-codearts/internal/auth"
	"go-codearts/internal/cnup"
	"go-codearts/internal/config"
	"go-codearts/internal/store"
)

// /v1/models 必须同时列出国内外模型，且带渠道前缀：
// global:<id> 走国际版账号，cn:<id> 走国内码道账号。
// 同名模型（如 GLM-5.2 两边都有）靠前缀区分，裸名不再输出。
func TestModelsListHasChannelPrefixes(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 国际账号：带一个模型
	if err := db.Upsert(&store.Account{
		ID: "intl-1", Email: "i@x.com", Channel: store.ChannelIntl,
		Models: []string{"GLM-5.2", "DeepSeek-R1"},
	}); err != nil {
		t.Fatal(err)
	}
	// 国内账号：目录由 SetAccountModels 注入（等价于登录/保活时拉到的结果）
	if err := db.Upsert(&store.Account{
		ID: "cn-1", Email: "c@x.com", Channel: store.ChannelCN,
		UserID:      "u-cn-1",
		AccessToken: "tok",
	}); err != nil {
		t.Fatal(err)
	}
	cnup.SetAccountModels("u-cn-1", []cnup.ModelInfo{
		{ID: "GLM-5.2"}, {ID: "DeepSeek-V3"}, {ID: "Qwen3-Coder"},
	})

	authMgr, err := auth.New("admin", "pass")
	if err != nil {
		t.Fatal(err)
	}
	s := New(config.Config{CNEnabled: true, DataDir: dir}, db, authMgr, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range out.Data {
		got[m.ID] = true
	}
	for _, want := range []string{"global:GLM-5.2", "global:DeepSeek-R1", "cn:GLM-5.2", "cn:DeepSeek-V3", "cn:Qwen3-Coder"} {
		if !got[want] {
			t.Fatalf("missing %q in /v1/models: %v", want, got)
		}
	}
	// 裸名不再输出：否则客户端无法判断选中了哪条链路。
	if got["GLM-5.2"] {
		t.Fatalf("bare model id leaked into /v1/models: %v", got)
	}
}

// 带前缀的模型必须能被调度侧解析回渠道，否则列表里的条目不可用。
func TestPrefixedModelResolvesRealm(t *testing.T) {
	cases := []struct{ in, realm, bare string }{
		{"global:GLM-5.2", store.ChannelIntl, "GLM-5.2"},
		{"cn:GLM-5.2", store.ChannelCN, "GLM-5.2"},
		{"intl:GLM-5.2", store.ChannelIntl, "GLM-5.2"},
		{"GLM-5.2", "", "GLM-5.2"},
	}
	for _, c := range cases {
		realm, bare := cnSplitChannelModel(c.in)
		if realm != c.realm || bare != c.bare {
			t.Fatalf("%q → (%q,%q), want (%q,%q)", c.in, realm, bare, c.realm, c.bare)
		}
	}
}

// 渠道字段缺失但带国内登录特征的账号，必须仍然显示为 cn:，
// 而不是被 NormalizeChannel("") 当成国际版塞进 global: 侧。
func TestModelsListInfersCNChannelFromCredentials(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Upsert(&store.Account{
		ID: "legacy-cn", Email: "l@x.com",
		// Channel 留空（旧数据）
		UserID:      "u-legacy",
		AccessToken: "tok",
		// 用一个内置兜底目录里绝不会出现的名字，才能区分「归到 cn 侧」
		// 与「同时命中国际侧兜底表」两种情况。
		Models: []string{"cn-only-fixture-model"},
	}); err != nil {
		t.Fatal(err)
	}
	authMgr, _ := auth.New("admin", "pass")
	s := New(config.Config{CNEnabled: true, DataDir: dir}, db, authMgr, nil)
	// 目录缓存为空时 /v1/models 会兜底拉一次；这里指向一个必然失败的地址，
	// 既走到兜底分支，又不让单元测试外呼真实上游。
	s.cnOnce.Do(func() {
		s.cnClient = cnup.NewWithChatEndpoint(200*time.Millisecond, "http://127.0.0.1:1")
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	got := map[string]bool{}
	for _, m := range out.Data {
		got[m.ID] = true
	}
	if !got["cn:cn-only-fixture-model"] {
		t.Fatalf("legacy cn account not listed under cn:: %v", got)
	}
	if got["global:cn-only-fixture-model"] {
		t.Fatalf("cn account leaked into global list: %v", got)
	}
}
