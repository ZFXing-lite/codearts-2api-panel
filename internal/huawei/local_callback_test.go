package huawei

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestLocalCallbackRejectsWrongState(t *testing.T) {
	cb, err := StartLocalCallback("expected-ticket")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()

	resp, err := http.Get(cb.URL() + "?code=abc&state=wrong-ticket")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := cb.Wait(ctx)
	if err == nil || result != nil {
		t.Fatalf("wrong state accepted: result=%#v err=%v", result, err)
	}
}

func TestLocalCallbackAcceptsExpectedState(t *testing.T) {
	cb, err := StartLocalCallback("expected-ticket")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()

	resp, err := http.Get(cb.URL() + "?code=abc&state=expected-ticket")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := cb.Wait(ctx)
	if err != nil || result == nil || result.Code != "abc" {
		t.Fatalf("expected callback result, got result=%#v err=%v", result, err)
	}
}
