package store

import (
	"path/filepath"
	"testing"
)

func TestAPIKeyAllowlist(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenAPIKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	kAll, secretAll, err := s.Create("all", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(kAll.AccountIDs) != 0 {
		t.Fatalf("empty allowlist want nil/empty got %v", kAll.AccountIDs)
	}
	kPick, secretPick, err := s.Create("test", []string{"acc-a", "", "acc-a", "acc-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(kPick.AccountIDs) != 2 || kPick.AccountIDs[0] != "acc-a" || kPick.AccountIDs[1] != "acc-b" {
		t.Fatalf("dedupe ids = %v", kPick.AccountIDs)
	}
	if !kAll.AllowsAccount("acc-z") {
		t.Fatal("empty allowlist should allow any account")
	}
	if !kPick.AllowsAccount("acc-a") || kPick.AllowsAccount("acc-z") {
		t.Fatal("restricted key allow check failed")
	}

	got := s.Lookup(secretPick)
	if got == nil || got.Name != "test" || len(got.AccountIDs) != 2 {
		t.Fatalf("lookup pick = %+v", got)
	}
	if s.Lookup(secretAll) == nil {
		t.Fatal("lookup all failed")
	}
	if s.Lookup("sk-hw-nope") != nil {
		t.Fatal("lookup invalid should be nil")
	}

	empty := []string{}
	updated, err := s.Update(kPick.ID, nil, &empty)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.AccountIDs) != 0 {
		t.Fatalf("clear allowlist got %v", updated.AccountIDs)
	}

	s2, err := OpenAPIKeys(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	listed := s2.List()
	if len(listed) != 2 {
		t.Fatalf("reload count=%d", len(listed))
	}
}

func TestAPIKeyAddUsagePersists(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenAPIKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	k, _, err := s.Create("cursor", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(k.ID, 100, 20); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(k.ID, 50, 5); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenAPIKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got *APIKey
	for _, it := range s2.List() {
		if it.ID == k.ID {
			got = it
		}
	}
	if got == nil {
		t.Fatal("missing key after reload")
	}
	if got.RequestCount != 2 || got.PromptTokens != 150 || got.CompletionTokens != 25 {
		t.Fatalf("usage=%+v", got)
	}
}
