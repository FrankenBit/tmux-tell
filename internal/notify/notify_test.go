package notify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// recvWithin reports whether a wake arrived on ch within d.
func recvWithin(t *testing.T, ch <-chan struct{}, d time.Duration) bool {
	t.Helper()
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// newTestWatcher points the runtime dir at a per-test temp dir and returns a
// started Watcher with cleanup wired (Close + the goleak guard depends on it).
func newTestWatcher(t *testing.T) *Watcher {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	w, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// TestWatchBeforeFirstNotifyWakes is the #515 D6 executable pin: a Watch
// registered BEFORE any Notify(key) ever fired must still wake on the very first
// Notify. This is the property the watch-the-directory design buys over a
// file-level watch — a file-level watch would have to Add a doorbell path that
// does not exist until the first ring, systematically missing first-contact.
//
// Mutation anchor: change NewWatcher's `fsw.Add(dir)` to add the per-key file
// instead, and this test fails (the Add errors on the missing path, or the
// create event is never observed) — reproducing the systematic first-message
// 5s-fallback the dir-watch design eliminates.
func TestWatchBeforeFirstNotifyWakes(t *testing.T) {
	w := newTestWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Watch first — the doorbell file does not exist yet.
	ch := w.Watch(ctx, "engineer")

	// First-ever ring: this CREATES <dir>/engineer.
	Notify("engineer")

	if !recvWithin(t, ch, 2*time.Second) {
		t.Fatal("Watch started before first Notify did not wake on the first Notify " +
			"(D6 dir-watch regressed to a file-level watch?)")
	}
}

// TestNotifyWatchRoundTrip is the basic happy path: a ring after the doorbell
// exists wakes the watcher.
func TestNotifyWatchRoundTrip(t *testing.T) {
	w := newTestWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := w.Watch(ctx, "bosun")
	Notify("bosun") // create
	if !recvWithin(t, ch, 2*time.Second) {
		t.Fatal("watcher did not wake on Notify")
	}
}

// TestRepeatedNotifyWakesAgain pins the re-arm property: a second ring to an
// ALREADY-EXISTING doorbell must wake again (the first ring's Create alone is
// not enough — every subsequent ring needs its own Modify event).
//
// Mutation anchor (verified): replace Notify's os.WriteFile with an
// open(O_CREATE)-then-close that neither truncates nor writes, and this fails
// (2s timeout) — re-opening an existing file without O_TRUNC or a write emits no
// inotify Modify, so the second ring is silently lost. os.WriteFile's
// O_CREATE|O_TRUNC is the load-bearing mechanism; the 1-byte payload is a
// cross-filesystem backstop (a write(2) generates Modify even where an
// already-zero-length O_TRUNC might not).
func TestRepeatedNotifyWakesAgain(t *testing.T) {
	w := newTestWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := w.Watch(ctx, "pilot")

	Notify("pilot") // create
	if !recvWithin(t, ch, 2*time.Second) {
		t.Fatal("watcher did not wake on first (create) Notify")
	}
	// Drain any coalesced extra, then ring the now-existing doorbell again.
	drain(ch)
	Notify("pilot") // write to existing file
	if !recvWithin(t, ch, 2*time.Second) {
		t.Fatal("watcher did not wake on second (write) Notify — payload may not generate a Modify event")
	}
}

// TestWatchKeyIsolation: Notify(A) must not wake a Watch(B). Negative timing
// assertion — local inotify delivers in single-digit ms, so a 400ms quiet
// window is comfortably generous.
func TestWatchKeyIsolation(t *testing.T) {
	w := newTestWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	other := w.Watch(ctx, "surveyor")
	Notify("carpenter") // a different key
	if recvWithin(t, other, 400*time.Millisecond) {
		t.Fatal("Watch(surveyor) woke on Notify(carpenter) — key demux leaked across recipients")
	}
}

// TestMultipleSubscribersSameKey: every subscriber on a key wakes (fan-out).
func TestMultipleSubscribersSameKey(t *testing.T) {
	w := newTestWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := w.Watch(ctx, "lookout")
	b := w.Watch(ctx, "lookout")
	Notify("lookout")
	if !recvWithin(t, a, 2*time.Second) {
		t.Fatal("first subscriber did not wake")
	}
	if !recvWithin(t, b, 2*time.Second) {
		t.Fatal("second subscriber did not wake")
	}
}

// TestCoalesce: a burst of rings collapses to a bounded, drainable signal — the
// buffered-1 channel never blocks the producer and the consumer is not forced to
// drain N times for N rings (it re-reads SQLite once per wake).
func TestCoalesce(t *testing.T) {
	w := newTestWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := w.Watch(ctx, "quartermaster")
	for i := 0; i < 50; i++ {
		Notify("quartermaster")
	}
	// At least one wake must arrive.
	if !recvWithin(t, ch, 2*time.Second) {
		t.Fatal("no wake after a burst of rings")
	}
	// Let the demux settle, then assert the channel holds at most one pending
	// wake (coalesced), not a backlog of ~50.
	time.Sleep(150 * time.Millisecond)
	extra := 0
	for recvWithin(t, ch, 50*time.Millisecond) {
		extra++
		if extra > 2 {
			t.Fatalf("burst of 50 rings did not coalesce: drained %d+ pending wakes", extra)
		}
	}
}

// TestNotifyEmptyKeyNoop: Notify("") is a guarded no-op (no file, no panic).
func TestNotifyEmptyKeyNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	Notify("")
	// No doorbell directory entry should have been created for an empty key.
	entries, _ := os.ReadDir(filepath.Join(dir, "tmux-tell", "notify"))
	for _, e := range entries {
		if e.Name() == "" {
			t.Fatal("empty key produced a doorbell file")
		}
	}
}

// TestSanitizeKeyRoundTrip: a key containing an unsafe separator still round-
// trips — Notify and Watch run the same sanitizer, so the producer filename and
// the consumer bucket agree and the wake is delivered.
func TestSanitizeKeyRoundTrip(t *testing.T) {
	w := newTestWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const key = "team/eng" // '/' would otherwise make a subdirectory
	ch := w.Watch(ctx, key)
	Notify(key)
	if !recvWithin(t, ch, 2*time.Second) {
		t.Fatal("sanitized key did not round-trip between Notify and Watch")
	}
}

// TestWatchUnsubscribesOnCtxDone: after a Watch ctx is cancelled, its
// subscriber is removed so a later ring does not leak through. (Also exercises
// the cleanup goroutine that goleak guards.)
func TestWatchUnsubscribesOnCtxDone(t *testing.T) {
	w := newTestWatcher(t)
	ctx, cancel := context.WithCancel(context.Background())

	ch := w.Watch(ctx, "herald")
	cancel()
	// Give the cleanup goroutine a moment to deregister.
	time.Sleep(100 * time.Millisecond)
	Notify("herald")
	if recvWithin(t, ch, 400*time.Millisecond) {
		t.Fatal("ring delivered to a Watch whose ctx was already cancelled")
	}
}

// TestCloseReleasesBackgroundCtxWatch pins the leak fix: a Watch made with a
// context whose Done() is a nil channel (context.Background) must still be
// released by Close — otherwise its cleanup goroutine selects on a nil channel
// and blocks forever. The goleak guard in TestMain is the actual assertion; if
// Close stops releasing such watches, the package test run fails on a leak.
func TestCloseReleasesBackgroundCtxWatch(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	w, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	_ = w.Watch(context.Background(), "engineer") // Done() == nil
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWatchOrNilRoundTrip: the best-effort consumer helper delivers wakes and
// its stop func tears the watch down (goleak guards the goroutines).
func TestWatchOrNilRoundTrip(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, stop := WatchOrNil(ctx, "bosun")
	defer stop()
	if ch == nil {
		t.Fatal("WatchOrNil returned a nil channel on a healthy setup")
	}
	Notify("bosun")
	if !recvWithin(t, ch, 2*time.Second) {
		t.Fatal("WatchOrNil channel did not wake on Notify")
	}
}

// drain empties any pending wake without blocking.
func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
