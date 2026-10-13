// Package watcher polls filesystem paths for change and fires callbacks with
// debounced coalescing. Pure stdlib (mtime + size), no fsnotify dependency —
// gains portability across Windows/Linux and every deployment environment we
// ship to, at the cost of poll latency. Two-second poll is well under
// operator-hot-reload expectations (edit config → sub-3s reload).
//
// Design intent (from CLIProxyAPI-inspired hot-reload plan):
//   - .env change → server rereads Rotate*/SSEIdleTimeout/HuaweiDispatchRPS/RespCacheTTL
//   - accounts.json change → db.Reload() + srv.syncShardFromDB()
//
// The callback is fired on the watcher goroutine; the caller is responsible
// for defer-recover / mutex discipline. Coalescing: multiple change events
// within debounce collapse into one callback with the LATEST mtime.
package watcher

import (
	"context"
	"os"
	"sync"
	"time"
)

// Watcher observes a set of files. Start it once; call Stop to release.
type Watcher struct {
	mu       sync.Mutex
	paths    map[string]state
	poll     time.Duration
	debounce time.Duration
	onChange func(path string)
	stop     chan struct{}
	done     chan struct{}
}

type state struct {
	mtime time.Time
	size  int64
	// pendingSince marks the first change we saw for this path within the
	// current debounce window. Cleared once the callback fires.
	pendingSince time.Time
}

// New constructs a Watcher. poll <= 0 defaults to 2s; debounce <= 0 defaults
// to 500ms. onChange fires with the path that changed, after the debounce
// window closes with no further changes.
func New(poll, debounce time.Duration, onChange func(path string)) *Watcher {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	if debounce <= 0 {
		debounce = 500 * time.Millisecond
	}
	return &Watcher{
		paths:    map[string]state{},
		poll:     poll,
		debounce: debounce,
		onChange: onChange,
	}
}

// Add registers a path. Baselines the current mtime so the FIRST subsequent
// mtime change fires the callback (i.e. the file's existing state does not
// trigger a spurious "changed" event on boot). Non-existent paths are watched
// too; creation counts as a change.
func (w *Watcher) Add(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.paths[path]; ok {
		return
	}
	st := state{}
	if fi, err := os.Stat(path); err == nil {
		st.mtime = fi.ModTime()
		st.size = fi.Size()
	}
	w.paths[path] = st
}

// Start begins polling. Callers get a handle to stop cleanly via ctx or Stop.
// Idempotent-ish: calling twice starts two goroutines — don't.
func (w *Watcher) Start(ctx context.Context) {
	w.stop = make(chan struct{})
	w.done = make(chan struct{})
	go w.loop(ctx)
}

// Stop halts the poller and waits for the goroutine to exit.
func (w *Watcher) Stop() {
	if w.stop == nil {
		return
	}
	close(w.stop)
	<-w.done
}

func (w *Watcher) loop(ctx context.Context) {
	defer close(w.done)
	t := time.NewTicker(w.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case now := <-t.C:
			w.tick(now)
		}
	}
}

// tick walks every registered path once, detects mtime/size changes, and
// fires debounced callbacks. mtime alone can miss same-second edits, so we
// treat any size delta as a change too. Reads that fail (file deleted mid-
// poll) are recorded as zero-state; the next successful read counts as a
// change (matches "recreated after delete" intuition).
func (w *Watcher) tick(now time.Time) {
	w.mu.Lock()
	// Snapshot to avoid holding mu during os.Stat (I/O can block on slow FS).
	snap := make(map[string]state, len(w.paths))
	for p, st := range w.paths {
		snap[p] = st
	}
	w.mu.Unlock()

	fires := make([]string, 0, len(snap))
	updates := make(map[string]state, len(snap))
	for p, prev := range snap {
		cur := state{pendingSince: prev.pendingSince}
		if fi, err := os.Stat(p); err == nil {
			cur.mtime = fi.ModTime()
			cur.size = fi.Size()
		}
		changed := !cur.mtime.Equal(prev.mtime) || cur.size != prev.size
		if changed {
			if cur.pendingSince.IsZero() {
				cur.pendingSince = now
			}
		}
		// Fire when debounce window has closed AND no fresh change is pending
		// past the current tick. We fire when `now - pendingSince >= debounce`
		// AND the current tick observed no NEW change vs the prev state.
		if !cur.pendingSince.IsZero() && !changed && now.Sub(cur.pendingSince) >= w.debounce {
			fires = append(fires, p)
			cur.pendingSince = time.Time{}
		}
		updates[p] = cur
	}

	w.mu.Lock()
	for p, st := range updates {
		w.paths[p] = st
	}
	w.mu.Unlock()

	for _, p := range fires {
		if w.onChange != nil {
			w.onChange(p)
		}
	}
}
