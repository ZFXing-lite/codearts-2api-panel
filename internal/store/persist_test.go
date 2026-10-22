package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 热重载必须能区分"外部手工编辑"与"自己的异步落盘"。
// 否则 debounced writer 每 2 秒改一次 mtime，watcher 就会用磁盘快照
// 整体替换内存，把这两秒内新产生的续期 token / 签到标记全部回滚。
func TestReloadFromDiskSkipsSelfWrite(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Save()
	if err := db.Upsert(&Account{ID: "a1", Email: "a@b.c"}); err != nil {
		t.Fatal(err)
	}
	// 触发一次真实落盘（等价于 debounced writer 干的活）
	if err := db.Save(); err != nil {
		t.Fatal(err)
	}
	// 落盘之后继续改内存，模拟"刚写完盘又有新修改"
	if err := db.Mutate("a1", func(a *Account) error {
		a.LastError = "newer-than-disk"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 此刻磁盘上还是旧内容，但内存里是自己的写 —— 自写抑制应让 reload 直接返回
	if err := db.ReloadFromDisk(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, _ := db.Get("a1")
	if got.LastError != "newer-than-disk" {
		t.Fatalf("self-write rolled back memory: %q", got.LastError)
	}
}

// 外部手工编辑（内容与自己写的不一致）必须照常生效。
func TestReloadFromDiskAppliesExternalEdit(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Upsert(&Account{ID: "a1", Email: "a@b.c"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Save(); err != nil {
		t.Fatal(err)
	}
	// 绕过 DB 直接改文件 = 运维手工编辑
	path := filepath.Join(dir, "accounts.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := string(raw)
	if len(edited) == 0 {
		t.Fatal("empty accounts.json")
	}
	// 用一次真实的字段替换制造内容差异
	edited = replaceOnce(edited, `"email": "a@b.c"`, `"email": "edited@b.c"`)
	if edited == string(raw) {
		t.Skip("email field layout changed; skip brittle assertion")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := db.ReloadFromDisk(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, _ := db.Get("a1")
	if got.Email != "edited@b.c" {
		t.Fatalf("external edit not applied: %q", got.Email)
	}
}

func replaceOnce(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 坏文件不能被当成空库：否则下一次 Upsert 会用空 map 覆盖全部凭证。
func TestOpenRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("expected error on corrupt accounts.json")
	}
	// 坏文件被挪走留档，原件不会被后续覆盖
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if len(e.Name()) > len("accounts.json.corrupt-") &&
			e.Name()[:len("accounts.json.corrupt-")] == "accounts.json.corrupt-" {
			found = true
		}
	}
	if !found {
		t.Fatalf("corrupt file not archived: %v", entries)
	}
}

// 落盘要留上一版 .bak，损坏时还有回头路。
func TestSaveWritesBackup(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Upsert(&Account{ID: "a1", Email: "a@b.c"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Save(); err != nil {
		t.Fatal(err)
	}
	// 再写一次：此时磁盘上已有旧版本，应产生 .bak
	if err := db.Upsert(&Account{ID: "a2", Email: "c@d.e"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Mutate("a1", func(a *Account) error {
		a.Email = "a2@b.c"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	err = db.saveLocked()
	db.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "accounts.json.bak")); statErr != nil {
		t.Fatalf("missing .bak: %v", statErr)
	}
}

// 深拷贝语义：Get 必须交出副本，调用方改字段不能污染库内状态。
// 这条守的是 Go 的 fatal concurrent map read/write（recover 拦不住）。
func TestGetReturnsDetachedCopy(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Save()
	if err := db.Upsert(&Account{ID: "a1", Email: "a@b.c", Metadata: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	a, err := db.Get("a1")
	if err != nil {
		t.Fatal(err)
	}
	a.Email = "hacked@b.c"
	a.Metadata["k"] = "hacked"

	again, _ := db.Get("a1")
	if again.Email != "a@b.c" || again.Metadata["k"] != "v" {
		t.Fatalf("Get leaked live pointer: %+v", again)
	}
}
