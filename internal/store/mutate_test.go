package store

import (
	"testing"
	"time"
)

func TestMutateAtomic(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(db.writeStop) }()
	db.Upsert(&Account{ID: "a1", Email: "x@x", RefreshToken: "R0"})

	// 正常改
	if err := db.Mutate("a1", func(a *Account) error {
		a.RefreshToken = "R1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	acc, _ := db.Get("a1")
	if acc.RefreshToken != "R1" {
		t.Fatalf("refresh_rotate=%s", acc.RefreshToken)
	}

	// 失败回滚
	err = db.Mutate("a1", func(a *Account) error {
		a.RefreshToken = "R-bad"
		a.Email = "hacked"
		return errForced
	})
	if err != errForced {
		t.Fatalf("expect err, got %v", err)
	}
	acc, _ = db.Get("a1")
	if acc.RefreshToken != "R1" || acc.Email != "x@x" {
		t.Fatalf("rollback failed: token=%s email=%s", acc.RefreshToken, acc.Email)
	}

	// UpdatedAt 被刷新
	before := acc.UpdatedAt
	time.Sleep(5 * time.Millisecond)
	db.Mutate("a1", func(a *Account) error { return nil })
	acc, _ = db.Get("a1")
	if !acc.UpdatedAt.After(before) {
		t.Fatal("UpdatedAt not bumped")
	}
}

var errForced = errDummy{}

type errDummy struct{}

func (errDummy) Error() string { return "forced" }

// TestRefreshLock 单飞语义：同账号只有一持。
func TestRefreshLock(t *testing.T) {
	db, _ := Open(t.TempDir())
	defer func() { close(db.writeStop) }()
	done := make(chan struct{})
	u1 := db.RefreshLock("acc-1")
	go func() { u2 := db.RefreshLock("acc-1"); u2(); close(done) }()
	select {
	case <-done:
		t.Fatal("second lock acquired while first held")
	case <-time.After(50 * time.Millisecond):
	}
	u1()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second lock not acquired after release")
	}
	// 不同账号互不阻塞
	la := db.RefreshLock("acc-a")
	lb := db.RefreshLock("acc-b")
	lb()
	la()
}
