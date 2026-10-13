package reqstats

import (
	"testing"
	"time"
)

func TestNoteEventAndHeaderWait(t *testing.T) {
	tr := New()
	tr.NoteEvent("client_abort")
	tr.NoteEvent("rotation")
	tr.NoteEvent("session_cap")
	tr.RecordHeaderWait(1200)
	snap := tr.Snapshot()
	if snap.Today.ClientAborts != 1 || snap.Today.Rotations != 1 || snap.Today.SessionCapHits != 1 {
		t.Fatalf("events=%+v", snap.Today)
	}
	if snap.Today.HeaderWaitP95Approx != 1200 {
		t.Fatalf("header p95=%d", snap.Today.HeaderWaitP95Approx)
	}
}

func TestHourBucketShanghai(t *testing.T) {
	utc := time.Date(2026, 9, 7, 1, 48, 0, 0, time.UTC)
	got := hourBucket(utc)
	want := time.Date(2026, 9, 7, 9, 0, 0, 0, statsLoc).UnixMilli()
	if got != want {
		t.Fatalf("hourBucket=%d label=%s want=%d", got, hourLabel(got), want)
	}
	if hourLabel(got) != "09:00" {
		t.Fatalf("label=%s", hourLabel(got))
	}
	if dayKey(utc) != "2026-09-07" {
		t.Fatalf("dayKey=%s", dayKey(utc))
	}
}
