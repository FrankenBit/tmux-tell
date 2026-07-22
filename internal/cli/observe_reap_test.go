package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestDueForReap pins the opt-in + cadence gate. The first row is the
// mutation-target for #836's "revert opt-in gate → sweep fires when it
// shouldn't": with enabled=false the sweep must never run, even when the
// interval has elapsed.
func TestDueForReap(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		enabled  bool
		last     time.Time
		interval time.Duration
		want     bool
	}{
		{"disabled — never, even when interval elapsed", false, now.Add(-time.Hour), time.Minute, false},
		{"enabled and interval elapsed", true, now.Add(-time.Hour), time.Minute, true},
		{"enabled but too soon", true, now.Add(-30 * time.Second), time.Minute, false},
		{"enabled, zero last → first tick due", true, time.Time{}, 6 * time.Hour, true},
	}
	for _, c := range cases {
		if got := dueForReap(c.enabled, c.last, now, c.interval); got != c.want {
			t.Errorf("%s: dueForReap = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestObserverReapSweep pins the fleet-wide sweep: it reaps dead-recipient
// fossils (agent=""), protects a live-pane recipient, and refuses an 'all'
// window. Reuses the phase-1 store predicate, so liveness protection is covered
// there; this asserts the observer wiring around it.
func TestObserverReapSweep(t *testing.T) {
	s := newCmdTestStore(t, "alice", "live") // live has pane %99 → protected
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "deadhost", Body: "f1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "deadhost", Body: "f2"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "live", Body: "protected"})

	var logbuf bytes.Buffer
	// `future` (from reset_test.go) so the 7d window includes the just-inserted rows.
	// nil metrics → exercises the nil-safe AddReaped path.
	n, err := observerReapSweep(ctx, s, "7d", future, nil, &logbuf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("reaped = %d, want 2 (deadhost×2; live protected)", n)
	}
	failed, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "deadhost", State: store.StateFailed})
	if len(failed) != 2 {
		t.Errorf("deadhost failed = %d, want 2 (dead-lettered)", len(failed))
	}
	liveQ, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "live", State: store.StateQueued})
	if len(liveQ) != 1 {
		t.Errorf("live queued = %d, want 1 (protected)", len(liveQ))
	}
	// 'all' window is refused — guard against reaping every queued row.
	if _, err := observerReapSweep(ctx, s, "all", future, nil, &logbuf); err == nil {
		t.Error("observerReapSweep with 'all' window: want error, got nil")
	}
}

// TestResolveAutoReap_Defaults pins the conservative defaults: OFF by default.
func TestResolveAutoReap_Defaults(t *testing.T) {
	got := resolveAutoReap(&config.File{})
	if got.enabled {
		t.Error("auto-reap default enabled = true, want false (opt-in only)")
	}
	if got.interval != config.DefaultAutoReapInterval {
		t.Errorf("interval = %v, want %v", got.interval, config.DefaultAutoReapInterval)
	}
	if got.olderThan != config.DefaultAutoReapOlderThan {
		t.Errorf("olderThan = %q, want %q", got.olderThan, config.DefaultAutoReapOlderThan)
	}
}
