package api

import (
	"strings"
	"testing"
)

func TestSanitizeFilterFinish(t *testing.T) {
	in := []byte(`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`)
	out, ok := sanitizeFilterFinish(in)
	if !ok {
		t.Fatal("expected rewrite")
	}
	if strings.Contains(string(out), "content_filter") {
		t.Fatalf("still has content_filter: %s", out)
	}
	if !strings.Contains(string(out), `"finish_reason":null`) && !strings.Contains(string(out), `"finish_reason": null`) {
		t.Fatalf("expected null finish_reason: %s", out)
	}
}

func TestIsProviderFilterFinish(t *testing.T) {
	for _, s := range []string{"content_filter", "content-filter", "refusal"} {
		if !isProviderFilterFinish(s) {
			t.Fatalf("expected filter: %s", s)
		}
	}
	if isProviderFilterFinish("stop") || isProviderFilterFinish("length") {
		t.Fatal("false positive")
	}
	// Content text must not be treated as a finish_reason.
	if isProviderFilterFinish("sensitive") || isProviderFilterFinish("safety") {
		t.Fatal("bare sensitive/safety should not match finish")
	}
}

func TestExtractUpstreamAssistText(t *testing.T) {
	in := []byte(`{"error_msg":"The current content involves sensitive information. Please try a new topic."}`)
	got := extractUpstreamAssistText(in)
	if !strings.Contains(got, "sensitive information") {
		t.Fatalf("got %q", got)
	}
}
