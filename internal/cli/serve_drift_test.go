package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestServe_DriftDetectionWithCanonicalAlias is the post-#38 form of
// the silent-drift regression: registered short name `surveyor`,
// drifted pane runs `--resume Surveyor` (capital S, treated here as
// an alias on the canonical row). The mailman should resolve the
// running name back to canonical via the alias and reroute.
func TestServe_DriftDetectionWithCanonicalAlias(t *testing.T) {
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		// %3 runs `surveyor` (canonical match); %4 runs `Pilot`.
		return []byte("%3\t300\t✳ Surveyor\tclaude\n" +
			"%4\t400\t✳ Pilot\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			switch pid {
			case 300:
				return "claude\x00--resume\x00surveyor\x00", nil
			case 400:
				return "claude\x00--resume\x00Pilot\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	var (
		bodyMu   sync.Mutex
		body     string
		paneSeen atomic.Value
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "load-buffer":
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				bodyMu.Lock()
				body = string(b)
				bodyMu.Unlock()
			}
		case "paste-buffer":
			for i, a := range args {
				if a == "-t" && i+1 < len(args) {
					paneSeen.Store(args[i+1])
				}
			}
		case "capture-pane":
			bodyMu.Lock()
			defer bodyMu.Unlock()
			return []byte(body), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	// surveyor's row is STALE: it points at %4, but %4 now runs Pilot (e.g. a
	// post-reboot pane renumber moved surveyor to %3 without a re-register).
	// Pre-#549 this test also registered pilot→%4 ("pilot registered
	// correctly") to model "surveyor drifted onto pilot's pane" — but Fix-2a's
	// one-pane-one-identity rule makes that simultaneous two-names-one-pane
	// state unreachable via UpsertAgent (whoever registers %4 last holds it
	// solely; the other row is released to NULL). The realistic drift this #37
	// guard exists for is a stale SINGLE registration whose pane now runs
	// someone else — exactly what remains here. Fix-2a and this guard compose:
	// Fix-2a PROACTIVELY prevents the mis-delivery at register time once the new
	// occupant registers; this guard REACTIVELY catches it while they haven't.
	_ = s.UpsertAgent(ctx, "surveyor", "%4") // drifted; %4 is Pilot now
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "hello",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", State: store.StateDelivered, Limit: 10,
		})
		if len(d) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	if seen := paneSeen.Load(); seen != "%3" {
		t.Errorf("paste-buffer pane = %v, want %%3 (drift should have rerouted via canonical); log=%s",
			seen, logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "drift_detected") {
		t.Errorf("expected drift_detected log; got %s", logbuf.String())
	}
}

// TestServe_SilentDriftDetectionAtDelivery is the regression for #37.
// Scenario: the registry says surveyor → %4, but %4 is actually
// Pilot's pane now (post-tmux-restore drift). With drift detection
// disabled, the mailman would deliver to %4 (Pilot's pane) and
// happily mark it delivered. With detection on, the mailman:
//
//  1. Reads the running agent from %4 via PaneAgentName → "Pilot".
//  2. Mismatch against opts.Agent="surveyor".
//  3. LookupByName("surveyor") returns %3 (where surveyor actually is).
//  4. UpsertAgent updates the registry: surveyor → %3.
//  5. Delivery proceeds to %3, not %4.
//
// We assert all five steps via the log lines and the final registry
// state.
func TestServe_SilentDriftDetectionAtDelivery(t *testing.T) {
	// Fake tmux: %3 is surveyor's pane (pid 300), %4 is Pilot's
	// pane (pid 400).
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%3\t300\t✳ Surveyor\tclaude\n" +
			"%4\t400\t✳ Pilot\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			switch pid {
			case 300:
				return "claude\x00--resume\x00surveyor\x00", nil
			case 400:
				return "claude\x00--resume\x00Pilot\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	// Fake delivery: paste-buffer + Enter succeed; verify-token check
	// finds whatever was written via load-buffer (mailman's standard
	// fake-runner pattern).
	var (
		bodyMu   sync.Mutex
		body     string
		paneSeen atomic.Value // tracks the pane id paste-buffer was called against
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "load-buffer":
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				bodyMu.Lock()
				body = string(b)
				bodyMu.Unlock()
			}
			return nil, nil
		case "paste-buffer":
			for i, a := range args {
				if a == "-t" && i+1 < len(args) {
					paneSeen.Store(args[i+1])
				}
			}
			return nil, nil
		case "send-keys", "delete-buffer":
			return nil, nil
		case "capture-pane":
			bodyMu.Lock()
			defer bodyMu.Unlock()
			return []byte(body), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4") // ← drifted: %4 belongs to Pilot now
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "hello",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false // ← opt into the check
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", State: store.StateDelivered, Limit: 10,
		})
		if len(d) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	// 1. Delivery succeeded.
	d, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "surveyor", State: store.StateDelivered, Limit: 10,
	})
	if len(d) != 1 {
		t.Fatalf("delivered = %d, want 1; log=%s", len(d), logbuf.String())
	}

	// 2. Delivery targeted %3 (surveyor's real pane), NOT %4 (Pilot's).
	if seen := paneSeen.Load(); seen != "%3" {
		t.Errorf("paste-buffer pane = %v, want %%3 (drift should have rerouted)", seen)
	}

	// 3. Registry was updated to point at the correct pane.
	agent, _ := s.GetAgent(ctx, "surveyor")
	if agent == nil || agent.PaneID != "%3" {
		t.Errorf("registry pane = %v, want %%3", agent)
	}

	// 4. drift_detected log line was emitted with the right fields.
	logs := logbuf.String()
	if !strings.Contains(logs, "drift_detected") {
		t.Errorf("expected drift_detected log line; got %s", logs)
	}
	if !strings.Contains(logs, "registered_pane=%4") {
		t.Errorf("log should name the drifted pane %%4; got %s", logs)
	}
	if !strings.Contains(logs, "rediscovered=%3") {
		t.Errorf("log should name the rediscovered pane %%3; got %s", logs)
	}
}

// TestServe_DriftUnrecoverable_FailLoud (v0.2.1 default): when drift
// is detected but the agent can't be relocated, MarkFailed rather
// than delivering to the wrong pane. The 2026-05-31 misdelivery
// class (silent-bad-delivery to wrong agent) MUST surface to the
// sender for autonomous receivers — Surveyor Q(b) review.
func TestServe_DriftUnrecoverable_FailLoud(t *testing.T) {
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		// %4 runs Pilot; surveyor isn't running anywhere.
		return []byte("%4\t400\t✳ Pilot\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			if pid == 400 {
				return "claude\x00--resume\x00Pilot\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4")
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "hi",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.DriftSoftFail = false // ← v0.2.1 default; fail-loud
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", State: store.StateFailed, Limit: 10,
		})
		if len(f) >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	failed, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "surveyor", State: store.StateFailed, Limit: 10,
	})
	if len(failed) != 1 {
		t.Errorf("failed = %d, want 1; log=%s", len(failed), logbuf.String())
	}
	delivered, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "surveyor", State: store.StateDelivered, Limit: 10,
	})
	if len(delivered) != 0 {
		t.Errorf("delivered = %d, want 0 (fail-loud should NOT deliver to wrong pane)",
			len(delivered))
	}
	if !strings.Contains(logbuf.String(), "drift_detected_unrecoverable") {
		t.Errorf("expected drift_detected_unrecoverable log; got %s", logbuf.String())
	}
	if len(failed) > 0 && !strings.Contains(failed[0].Error.String, "drift_detected_unrecoverable") {
		t.Errorf("failed reason should name the drift class; got %q", failed[0].Error.String)
	}
}

// TestServe_DriftUnrecoverable_SoftFailEscapeHatch: with --drift-soft-fail
// the pre-v0.2.1 behaviour is preserved — log WARN and deliver to the
// drifted pane.
func TestServe_DriftUnrecoverable_SoftFailEscapeHatch(t *testing.T) {
	// Live pane %4 runs Pilot; surveyor isn't running anywhere.
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%4\t400\t✳ Pilot\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			if pid == 400 {
				return "claude\x00--resume\x00Pilot\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		// Make paste-buffer at %4 succeed (existing test shape).
		switch args[0] {
		case "load-buffer", "paste-buffer", "send-keys", "delete-buffer":
			return nil, nil
		case "capture-pane":
			// Empty content; verify-token won't match → ErrUnverifiedDelivery.
			return []byte("\n"), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })
	prevRetry := tmuxio.SetRetrySchedule([]time.Duration{time.Microsecond})
	t.Cleanup(func() { tmuxio.SetRetrySchedule(prevRetry) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4")
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "hi",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.DriftSoftFail = true // ← escape hatch
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// Either delivered (verified or unverified) is fine — the
		// point is we don't crash AND we don't fail-mark.
		all, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", Limit: 10,
		})
		if len(all) >= 1 {
			done := false
			for _, m := range all {
				if m.State == store.StateDelivered || m.State == store.StateFailed {
					done = true
					break
				}
			}
			if done {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	if !strings.Contains(logbuf.String(), "drift_detected_unrecoverable") {
		t.Errorf("expected drift_detected_unrecoverable WARN log; got %s", logbuf.String())
	}
	failed, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "surveyor", State: store.StateFailed, Limit: 10,
	})
	if len(failed) != 0 {
		t.Errorf("soft-fail should NOT MarkFailed; got %d failed", len(failed))
	}
}

// TestServe_BareShell_NoLiveSession_BlocksUnconditionally is the #626
// Phase 1a safety regression: the registered pane outlived its session and
// now hosts a bare shell (no claude process, generic window name). Pasting
// the message body there would execute it as a shell command. The mailman
// must BLOCK (MarkFailed, never paste) when the addressed session is found
// in NO pane -- and unconditionally, even under --drift-soft-fail (the
// bare-shell block is a safety invariant, distinct from the
// deliver-to-wrong-agent policy that --drift-soft-fail governs).
func TestServe_BareShell_NoLiveSession_BlocksUnconditionally(t *testing.T) {
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		// %4 is a bare shell: no claude, empty title, generic window name.
		return []byte("%4\t400\t\tbash\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			if pid == 400 {
				return "bash\x00", nil // bare shell -- no --resume
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	var pasted atomic.Bool
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "paste-buffer" {
			pasted.Store(true)
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4") // stale: %4 is a bare shell now
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "rm -rf important",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.DriftSoftFail = true // even with the escape hatch, bare-shell blocks
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", State: store.StateFailed, Limit: 10,
		})
		if len(f) >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	failed, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "surveyor", State: store.StateFailed, Limit: 10,
	})
	if len(failed) != 1 {
		t.Fatalf("failed = %d, want 1 (bare shell must block); log=%s", len(failed), logbuf.String())
	}
	delivered, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "surveyor", State: store.StateDelivered, Limit: 10,
	})
	if len(delivered) != 0 {
		t.Errorf("delivered = %d, want 0 (NEVER paste to a bare shell)", len(delivered))
	}
	if pasted.Load() {
		t.Errorf("paste-buffer was called -- message was pasted into the bare shell (#626 gap)")
	}
	if !strings.Contains(logbuf.String(), "no_live_session") {
		t.Errorf("expected no_live_session log; got %s", logbuf.String())
	}
	if len(failed) > 0 && !strings.Contains(failed[0].Error.String, "no_live_session") {
		t.Errorf("failed reason should name no_live_session; got %q", failed[0].Error.String)
	}
}

// TestServe_BareShell_SessionRelocated_Reroutes: the registered pane went
// bare-shell, but the addressed session is alive in a DIFFERENT pane
// (operator restarted it elsewhere). The mailman re-discovers it across
// panes, reroutes delivery, and heals the registry -- rather than blocking.
func TestServe_BareShell_SessionRelocated_Reroutes(t *testing.T) {
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		// %4 is a bare shell; %5 now runs surveyor.
		return []byte("%4\t400\t\tbash\n" +
			"%5\t500\tSurveyor\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			switch pid {
			case 400:
				return "bash\x00", nil
			case 500:
				return "claude\x00--resume\x00surveyor\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	var (
		bodyMu   sync.Mutex
		body     string
		paneSeen atomic.Value
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "load-buffer":
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				bodyMu.Lock()
				body = string(b)
				bodyMu.Unlock()
			}
		case "paste-buffer":
			for i, a := range args {
				if a == "-t" && i+1 < len(args) {
					paneSeen.Store(args[i+1])
				}
			}
		case "capture-pane":
			bodyMu.Lock()
			defer bodyMu.Unlock()
			return []byte(body), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4") // stale: %4 bare-shell; surveyor moved to %5
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "hello",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", State: store.StateDelivered, Limit: 10,
		})
		if len(d) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	d, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "surveyor", State: store.StateDelivered, Limit: 10,
	})
	if len(d) != 1 {
		t.Fatalf("delivered = %d, want 1 (should reroute to the relocated session); log=%s", len(d), logbuf.String())
	}
	if seen := paneSeen.Load(); seen != "%5" {
		t.Errorf("paste-buffer pane = %v, want %%5 (relocated)", seen)
	}
	agent, _ := s.GetAgent(ctx, "surveyor")
	if agent == nil || agent.PaneID != "%5" {
		t.Errorf("registry pane = %v, want %%5 (healed)", agent)
	}
	if !strings.Contains(logbuf.String(), "session_relocated") {
		t.Errorf("expected session_relocated log; got %s", logbuf.String())
	}
}

// TestServe_BareShell_LookupError_BlocksUnconditionally is the Surveyor
// review-3287 regression: reaching running=="" means the registered pane is
// CONFIRMED bare (outer probe read cleanly). If the across-pane relocation
// lookup then ERRORS, the addressed session was NOT positively located, so
// pasting into the registered pane still means pasting into a bare shell.
// The block must fire here too -- even at DriftSoftFail=false (default),
// where the pre-fix code only logged and fell through to the paste.
func TestServe_BareShell_LookupError_BlocksUnconditionally(t *testing.T) {
	var calls atomic.Int32
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		// 1st call (outer drift probe): %4 is a bare shell -> running=="".
		// Subsequent call (the across-pane relocation lookup): env error.
		if calls.Add(1) == 1 {
			return []byte("%4\t400\t\tbash\n"), nil
		}
		return nil, errors.New("list-panes failed")
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			if pid == 400 {
				return "bash\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	var pasted atomic.Bool
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "paste-buffer" {
			pasted.Store(true)
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4")
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "rm -rf important",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.DriftSoftFail = false // default; the pre-fix lerr leak fired even here
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", State: store.StateFailed, Limit: 10,
		})
		if len(f) >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	failed, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "surveyor", State: store.StateFailed, Limit: 10,
	})
	if len(failed) != 1 {
		t.Fatalf("failed = %d, want 1 (lookup-error from a confirmed-bare pane must block); log=%s", len(failed), logbuf.String())
	}
	if pasted.Load() {
		t.Errorf("paste-buffer was called -- lookup-error fell through to a bare-shell paste (the review-3287 hole)")
	}
	if !strings.Contains(logbuf.String(), "no_live_session") {
		t.Errorf("expected no_live_session log; got %s", logbuf.String())
	}
}

// TestServe_BareShell_LookupAmbiguous_BlocksUnderSoftFail: registered pane is
// bare, and the relocation lookup is ambiguous (another pane's --resume value
// substring-matches >1 canonical). Pre-fix this set an OVERRIDABLE
// driftFailReason, so --drift-soft-fail bypassed the guard and pasted into the
// bare shell. A bare-shell paste must never be soft-fail-overridable.
func TestServe_BareShell_LookupAmbiguous_BlocksUnderSoftFail(t *testing.T) {
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		// %4 bare (registered for surveyor); %5 runs `--resume bosun`, which
		// substring-matches both canonicals "bo" and "bos" -> ambiguous lookup.
		return []byte("%4\t400\t\tbash\n" +
			"%5\t500\tbosun\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			switch pid {
			case 400:
				return "bash\x00", nil
			case 500:
				return "claude\x00--resume\x00bosun\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	var pasted atomic.Bool
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "paste-buffer" {
			pasted.Store(true)
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4")
	_ = s.UpsertAgent(ctx, "bo", "%7")  // canonical; substring of "bosun"
	_ = s.UpsertAgent(ctx, "bos", "%8") // canonical; substring of "bosun" -> 2 matches = ambiguous
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "rm -rf important",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.DriftSoftFail = true // the escape hatch must NOT bypass the bare-shell block
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", State: store.StateFailed, Limit: 10,
		})
		if len(f) >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	failed, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "surveyor", State: store.StateFailed, Limit: 10,
	})
	if len(failed) != 1 {
		t.Fatalf("failed = %d, want 1 (ambiguous lookup from a bare pane must block even under --drift-soft-fail); log=%s", len(failed), logbuf.String())
	}
	if pasted.Load() {
		t.Errorf("paste-buffer was called -- ambiguous lookup was soft-fail-overridden into a bare-shell paste (the review-3287 hole)")
	}
	if !strings.Contains(logbuf.String(), "no_live_session") {
		t.Errorf("expected no_live_session log; got %s", logbuf.String())
	}
}
