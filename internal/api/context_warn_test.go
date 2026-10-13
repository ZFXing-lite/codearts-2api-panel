package api

import (
	"net/http/httptest"
	"testing"
)

func TestWarnIfContextLarge_UnderThreshold_NoHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	warnIfContextLarge(rec, "sess1", contextWarnTokens-1)
	if rec.Header().Get("X-Context-Warning") != "" {
		t.Fatalf("must not warn below threshold, got headers %v", rec.Header())
	}
	if rec.Header().Get("X-Context-Tokens-Estimate") != "" {
		t.Fatalf("must not set token estimate header below threshold, got headers %v", rec.Header())
	}
}

func TestWarnIfContextLarge_AtAndAboveThreshold_SetsHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	warnIfContextLarge(rec, "sess2", contextWarnTokens)
	if got := rec.Header().Get("X-Context-Warning"); got != "approaching_limit" {
		t.Fatalf("want approaching_limit at threshold, got %q", got)
	}
	if got := rec.Header().Get("X-Context-Tokens-Estimate"); got != "250000" {
		t.Fatalf("want exact token estimate at threshold, got %q", got)
	}

	rec2 := httptest.NewRecorder()
	warnIfContextLarge(rec2, "sess3", 300_000)
	if got := rec2.Header().Get("X-Context-Tokens-Estimate"); got != "300000" {
		t.Fatalf("want exact token estimate above threshold, got %q", got)
	}
}
