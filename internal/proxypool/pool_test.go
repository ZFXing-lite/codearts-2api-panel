package proxypool

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUpsertListDelete(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.Upsert(Proxy{
		Type:    "socks5h",
		Host:    "127.0.0.1",
		Port:    1080,
		Enabled: true,
		Name:    "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == "" {
		t.Fatal("expected stable id")
	}
	list := st.List()
	if len(list) != 1 {
		t.Fatalf("list=%d", len(list))
	}
	got, err := st.Get(p.ID)
	if err != nil || got.Host != "127.0.0.1" {
		t.Fatalf("get: %+v %v", got, err)
	}
	// reopen persistence
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.List()) != 1 {
		t.Fatal("persist fail")
	}
	if err := st2.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if len(st2.List()) != 0 {
		t.Fatal("delete fail")
	}
	_ = filepath.Join(dir, "proxies.json")
}

func TestRoundRobinAndCooldown(t *testing.T) {
	dir := t.TempDir()
	pool, err := OpenPool(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := pool.Upsert(Proxy{Type: "http", Host: "10.0.0.1", Port: 8080, Enabled: true, Name: "a"})
	b, _ := pool.Upsert(Proxy{Type: "http", Host: "10.0.0.2", Port: 8080, Enabled: true, Name: "b"})
	_ = pool.SetStrategy(StrategyRoundRobin)

	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		px, err := pool.Store().Next("")
		if err != nil {
			t.Fatal(err)
		}
		seen[px.ID]++
	}
	if seen[a.ID] == 0 || seen[b.ID] == 0 {
		t.Fatalf("rr distribution %+v", seen)
	}

	pool.Store().MarkFailure(a.ID, "boom", time.Hour)
	pool.Store().MarkFailure(a.ID, "boom", time.Hour)
	pool.Store().MarkFailure(a.ID, "boom", time.Hour)
	got, err := pool.Store().Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Usable(time.Now()) {
		t.Fatal("expected cooldown")
	}
	// only b remains usable
	for i := 0; i < 3; i++ {
		px, err := pool.Store().Next("")
		if err != nil {
			t.Fatal(err)
		}
		if px.ID != b.ID {
			t.Fatalf("want b got %s", px.ID)
		}
	}
}

func TestNormalizeStableID(t *testing.T) {
	p1 := Normalize(Proxy{Type: "SOCKS5H", Host: "X.COM", Port: 1, Username: "u"})
	p2 := Normalize(Proxy{Type: "socks5h", Host: "x.com", Port: 1, Username: "u"})
	if p1.ID != p2.ID {
		t.Fatalf("id mismatch %s vs %s", p1.ID, p2.ID)
	}
	if Validate(Proxy{Type: "ftp", Host: "h", Port: 1}) == nil {
		t.Fatal("expected validate error")
	}
}
