package huawei

import (
	"net/http"
	"testing"
)

func TestEndpointObfuscation(t *testing.T) {
	cases := map[string]string{
		"AuthPortalEndpoint":      "https://codearts.ap-southeast-1.huaweicloud.com/portal",
		"AuthTokenEndpoint":       "https://sts.cn-north-4.myhuaweicloud.com",
		"AuthTicketTokenEndpoint": "https://snap-access.cn-north-4.myhuaweicloud.com/snap-manager",
		"DefaultModelBase":        "https://snap-access.ap-southeast-1.myhuaweicloud.com",
		"OAuthClientID":           "CodeArts_Tui",
		"OAuthCallbackPath":       "/oauth/callback",
		"PathQuotaPlugin":         "/snap-manager/v1/statistics/plugin",
		"PathChatCompletions":     "/api/v2/chat/completions",
		"SDKHMACSHA256":           "SDK-HMAC-SHA256",
	}
	got := map[string]string{
		"AuthPortalEndpoint":      AuthPortalEndpoint,
		"AuthTokenEndpoint":       AuthTokenEndpoint,
		"AuthTicketTokenEndpoint": AuthTicketTokenEndpoint,
		"DefaultModelBase":        DefaultModelBase,
		"OAuthClientID":           OAuthClientID,
		"OAuthCallbackPath":       OAuthCallbackPath,
		"PathQuotaPlugin":         PathQuotaPlugin,
		"PathChatCompletions":     PathChatCompletions,
		"SDKHMACSHA256":           SDKHMACSHA256,
	}
	for k, want := range cases {
		if got[k] != want {
			t.Fatalf("%s: got %q want %q", k, got[k], want)
		}
	}
}

func TestCanonicalURIUsesCLITrailingSlash(t *testing.T) {
	if got := canonicalURI("/api/v2/chat/completions"); got != "/api/v2/chat/completions/" {
		t.Fatalf("canonical uri=%q", got)
	}
	if got := canonicalURI("/"); got != "/" {
		t.Fatalf("root canonical uri=%q", got)
	}
}

func TestMarkChatConnClose(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	markChatConnClose(req)
	if req.Close {
		t.Fatal("chat request must NOT close TCP — connection reuse enabled for speed")
	}
}
