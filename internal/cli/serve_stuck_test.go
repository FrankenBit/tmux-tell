package cli

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestServe_PaneNotFound_BacksOffAndParks is the #291 AC-1 test: an agent
// registered on a pane that does not exist receives a message; the mailman
// must NOT hammer tmux at ~100/s. Instead it backs off exponentially and,
// after StuckThreshold consecutive failures, parks itself (stuck_reason set)
// and stops probing entirely. The message stays queued — no data loss.
func TestServe_PaneNotFound_BacksOffAndParks(t *testing.T) {
	var mu sync.Mutex
	captureCalls := 0
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		if len(args) > 0 && args[0] == "capture-pane" {
			captureCalls++
		}
		mu.Unlock()
		// Every probe fails as if the pane is gone (the #288/#290 stale-%0 case).
		return nil, &errString{"exit status 1: can't find pane: %3"}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	r, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "to the void"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	opts := fastOpts("bob")
	opts.PrePasteSafetyDisabled = false // enable the probe that detects pane-not-found
	opts.StuckThreshold = 2             // fail1 (1s backoff) → fail2 → park
	opts.StuckPollInterval = 5 * time.Millisecond

	stop, wait, logbuf := runServeInBackground(t, s, opts)

	// Poll until the agent parks (or give up after a generous deadline).
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := s.GetAgent(ctx, "bob")
		if a.StuckReason != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Snapshot the probe count at park, then wait several stuck-poll cycles to
	// confirm probing has truly stopped (a parked mailman issues no tmux calls).
	mu.Lock()
	callsAtPark := captureCalls
	mu.Unlock()
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	callsAfterPark := captureCalls
	mu.Unlock()
	stop()
	wait()

	a, _ := s.GetAgent(ctx, "bob")
	if a.StuckReason != store.StuckReasonPaneNotFound {
		t.Fatalf("stuck_reason = %q, want %q", a.StuckReason, store.StuckReasonPaneNotFound)
	}
	// Bounded rate: with backoff + a threshold of 2, only a couple of probes
	// fire before parking. The pre-fix storm would be hundreds-to-thousands in
	// the same wall-clock. A generous bound still proves the storm is gone.
	if callsAtPark > 8 {
		t.Errorf("capture-pane probes before park = %d, want a small bounded count (storm not contained)", callsAtPark)
	}
	// Parked = no further probing.
	if callsAfterPark != callsAtPark {
		t.Errorf("capture-pane probes continued after park: %d → %d (parked mailman must not probe)", callsAtPark, callsAfterPark)
	}
	// No data loss: the message reverted to queued, never delivered/failed.
	final, _ := s.GetMessage(ctx, r.PublicID)
	if final.State != store.StateQueued {
		t.Errorf("message state = %s, want queued (parked, retained)", final.State)
	}
	for _, want := range []string{"pane_not_found_backoff", "stuck"} {
		if !strings.Contains(logbuf.String(), want) {
			t.Errorf("log missing %q; got:\n%s", want, logbuf.String())
		}
	}
}

// TestServe_StuckClearsAndResumes covers the #291 AC-2 recovery cycle and the
// AC4 clear path at the loop level: a parked mailman issues zero probes, and
// clearing stuck_reason (what `register --force` does) resumes delivery on the
// next loop. Using StuckThreshold=1 parks on the first failure so the test does
// not pay the backoff wall-clock.
func TestServe_StuckClearsAndResumes(t *testing.T) {
	var mu sync.Mutex
	captureCalls := 0
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		if len(args) > 0 && args[0] == "capture-pane" {
			captureCalls++
		}
		mu.Unlock()
		return nil, &errString{"exit status 1: can't find pane: %3"}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"})

	opts := fastOpts("bob")
	opts.PrePasteSafetyDisabled = false
	opts.StuckThreshold = 1 // park immediately on the first failure (no backoff wait)
	opts.StuckPollInterval = 5 * time.Millisecond

	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	// Wait until parked.
	waitFor(t, 2*time.Second, func() bool {
		a, _ := s.GetAgent(ctx, "bob")
		return a.StuckReason != ""
	}, "agent did not park")

	mu.Lock()
	parkedCalls := captureCalls
	mu.Unlock()
	time.Sleep(60 * time.Millisecond) // several stuck-poll cycles
	mu.Lock()
	quietCalls := captureCalls
	mu.Unlock()
	if quietCalls != parkedCalls {
		t.Fatalf("parked mailman kept probing: %d → %d", parkedCalls, quietCalls)
	}

	// Operator fixes the registration: clear the stuck state (the register
	// --force effect). The mailman must resume probing on its next loop.
	if err := s.ClearStuck(ctx, "bob"); err != nil {
		t.Fatalf("clear stuck: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return captureCalls > quietCalls
	}, "mailman did not resume probing after stuck cleared")
}

// TestRegister_ClearsStuckState pins the AC4 CLI surface: `register --force`
// on a parked agent clears stuck_reason (the operator-fixes-registration path).
func TestRegister_ClearsStuckState(t *testing.T) {
	db := filepath.Join(t.TempDir(), "messages.db")
	ctx := context.Background()

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if err := s.SetStuck(ctx, "bob", store.StuckReasonPaneNotFound); err != nil {
		t.Fatalf("seed stuck: %v", err)
	}
	_ = s.Close()

	var stdout, stderr strings.Builder
	exit := runRegisterCLI(
		[]string{"--db", db, "--name", "bob", "--pane", "%3", "--force", "--start-mailman=false"},
		&stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("register exit = %d, want %d; stderr=%s", exit, exitOK, stderr.String())
	}

	s2, _ := store.Open(db)
	t.Cleanup(func() { _ = s2.Close() })
	a, err := s2.GetAgent(ctx, "bob")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.StuckReason != "" {
		t.Errorf("stuck_reason = %q after register --force, want cleared", a.StuckReason)
	}
}

// TestPaneNotFoundBackoff_Schedule pins the load-bearing #291 invariant: the
// retry delay grows 1s → 2s → 4s → … and is capped at 60s. This cap is what
// converts the ~100/s storm into at most 1/60s. A mutation that uncaps or
// flattens this schedule is what reopens the tmux-wedge risk.
func TestPaneNotFoundBackoff_Schedule(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, time.Second}, // guarded up to 1
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 32 * time.Second},
		{7, 60 * time.Second}, // 1s<<6 = 64s, capped to 60s
		{8, 60 * time.Second},
		{100, 60 * time.Second}, // no overflow on a large counter
	}
	for _, c := range cases {
		if got := paneNotFoundBackoff(c.n); got != c.want {
			t.Errorf("paneNotFoundBackoff(%d) = %s, want %s", c.n, got, c.want)
		}
	}
}

// TestServe_CounterResetOnProbeRecovery pins #299: the in-memory
// consecutive-fail counter resets after a non-can't-find-pane probe
// abort (lines 957-961 in serve.go), so the mailman requires a FULL
// StuckThreshold consecutive can't-find-pane failures post-recovery
// before parking. Without that reset, one extra can't-find-pane failure
// after a mixed-error interlude would immediately tip the counter over
// the threshold — the message ID check alone wouldn't save it because
// the same message keeps being re-claimed across the phase boundary.
//
// The three-phase runner:
//
//	Phase 1 — failsBeforeRecover "can't find pane" probes: counter builds
//	           up to StuckThreshold-1 (no park yet).
//	Phase 2 — one non-can't-find-pane error: lines 957-961 reset counter
//	           to 0 and clear paneFailMsgID.
//	Phase 3 — "can't find pane" again: needs a full StuckThreshold probes
//	           before parking (not just one, as a missing reset would allow).
//
// setStuckBackoffBaseForTest(time.Millisecond) (the #299 test seam) keeps
// the total wall-clock under ~50ms without losing structural coverage of
// the backoff schedule.
func TestServe_CounterResetOnProbeRecovery(t *testing.T) {
	const (
		threshold          = 4
		failsBeforeRecover = threshold - 1 // builds a streak without parking
	)

	prevBase := setStuckBackoffBaseForTest(time.Millisecond)
	t.Cleanup(func() { setStuckBackoffBaseForTest(prevBase) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	var mu sync.Mutex
	probeCount := 0   // total capture-pane calls (one per failing AgentState probe)
	phase3Probes := 0 // count of Phase-3 probes, to assert full threshold was needed

	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if len(args) == 0 || args[0] != "capture-pane" {
			return nil, nil
		}
		mu.Lock()
		probeCount++
		n := probeCount
		mu.Unlock()

		switch {
		case n <= failsBeforeRecover:
			// Phase 1: build up a can't-find-pane streak.
			return nil, &errString{"exit status 1: can't find pane: %3"}
		case n == failsBeforeRecover+1:
			// Phase 2: a single non-can't-find-pane error triggers the
			// non-pane-not-found abort path (lines 957-961) which resets
			// consecutivePaneFails=0 and paneFailMsgID="".
			return nil, &errString{"exit status 1: tmux: socket error (not pane-not-found)"}
		default:
			// Phase 3: pane gone again. Must fire >= threshold can't-find-pane
			// probes before parking (counter was reset in Phase 2, not at threshold-1).
			mu.Lock()
			phase3Probes++
			mu.Unlock()
			return nil, &errString{"exit status 1: can't find pane: %3"}
		}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	r, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"})

	opts := fastOpts("bob")
	opts.PrePasteSafetyDisabled = false
	opts.StuckThreshold = threshold
	opts.StuckPollInterval = 5 * time.Millisecond

	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	waitFor(t, 4*time.Second, func() bool {
		a, _ := s.GetAgent(ctx, "bob")
		return a.StuckReason != ""
	}, "agent did not park")

	mu.Lock()
	p3 := phase3Probes
	mu.Unlock()

	// Phase 3 must have fired a full threshold of can't-find-pane probes.
	// Without the reset at lines 957-961, the counter would be at threshold-1
	// entering Phase 3 and park on just one more failure (p3 would be 1).
	if p3 < threshold {
		t.Errorf("phase3Probes = %d, want >= %d; counter was not reset before Phase 3 (lines 957-961 broken)", p3, threshold)
	}

	a, _ := s.GetAgent(ctx, "bob")
	if a.StuckReason != store.StuckReasonPaneNotFound {
		t.Errorf("stuck_reason = %q, want %q", a.StuckReason, store.StuckReasonPaneNotFound)
	}
	// No data loss: message is still queued while parked.
	m, _ := s.GetMessage(ctx, r.PublicID)
	if m.State != store.StateQueued {
		t.Errorf("message state = %s, want queued (retained while parked)", m.State)
	}
}

// waitFor polls cond until it returns true or the timeout elapses, failing the
// test with msg on timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatal(msg)
}
