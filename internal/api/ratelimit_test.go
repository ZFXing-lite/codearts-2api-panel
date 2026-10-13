package api

import (
	"testing"

	"go-codearts/internal/store"
)

func TestKeyGateInFlightAndRPM(t *testing.T) {
	g := newKeyGate(2, 1)
	if !g.Acquire("k1", 0) {
		t.Fatal("first acquire")
	}
	if g.Acquire("k1", 0) {
		t.Fatal("in-flight cap=1")
	}
	g.Release("k1")
	if !g.Acquire("k1", 0) {
		t.Fatal("after release")
	}
	g.Release("k1")
	if g.Acquire("k1", 0) {
		t.Fatal("rpm cap=2")
	}
}

func TestKeyGateDisabled(t *testing.T) {
	g := newKeyGate(0, 0)
	if !g.Acquire("k", 0) || !g.Acquire("k", 0) {
		t.Fatal("disabled gate must allow")
	}
}

func TestKeyGateShareCarvesFromGlobal(t *testing.T) {
	g := newKeyGate(0, 8)
	if !g.Acquire("a", 2) || !g.Acquire("a", 2) {
		t.Fatal("share allows 2")
	}
	if g.Acquire("a", 2) {
		t.Fatal("third must be carved by share=2")
	}
}

func TestKeyInFlightShareSplitsSem(t *testing.T) {
	dir := t.TempDir()
	keys, err := store.OpenAPIKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := keys.Create("a", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := keys.Create("b", nil); err != nil {
		t.Fatal(err)
	}
	s := &Server{apiKeys: keys, chatSem: make(chan struct{}, 32)}
	if got := s.keyInFlightShare(); got != 16 {
		t.Fatalf("share=%d want 16", got)
	}
}
