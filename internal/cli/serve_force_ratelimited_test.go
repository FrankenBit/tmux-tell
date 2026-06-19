package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/metrics"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestSend_MultiRecipient_ForceRateLimitedRejected pins the #558 fail-loud guard
// (Lookout #574 review): --force-rate-limited is a deliberate operator
// escape-hatch, and the fan-out InsertParams doesn't carry the per-message
// marker, so a multi-recipient send must REJECT it rather than silently drop it.
// Matches the existing --deliver-after single-recipient-only guard.
func TestSend_MultiRecipient_ForceRateLimitedRejected(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	exit := runSendWithStore(ctx, s, sendParams{
		From:             "alice",
		ToRecipients:     []string{"bob", "carol"},
		Body:             "force the broadcast",
		ForceRateLimited: true,
		MaxRecipient:     5,
		MaxSender:        10,
		MaxBody:          1024,
	}, &stdout, &stderr)
	if exit == exitOK {
		t.Errorf("expected non-zero exit: --force-rate-limited must fail-loud on multi-send, not silently drop")
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != false {
		t.Errorf("ok = %v, want false", got["ok"])
	}
	if errStr, _ := got["error"].(string); !strings.Contains(errStr, "force-rate-limited is single-recipient only") {
		t.Errorf("error = %q, want the single-recipient-only message", errStr)
	}
}

// TestServe_ForceRateLimited_DeliversThroughRateLimitDefer is the #558 AC pin and
// the two-site mutation anchor. A message sent with --force-rate-limited reaches a
// pane showing a rate-limit banner; it must deliver anyway. This exercises BOTH
// force sites: the observe-gate (which would return ErrRateLimited) and the #105
// pre-paste re-probe (which re-reads the banner as StateRateLimited and, via
// IsPasteUnsafe, would otherwise re-block what the gate just waved through).
//
// The stateful fake returns the banner until the paste lands (so gate + #105 both
// see rate-limited), then echoes the pasted body so the delivery verifies.
//
// Mutation anchors:
//   - serve.go gate switch: drop the `if msg.ForceRateLimited { break }` → the
//     message defers on the backoff, never delivers → this fails.
//   - serve.go #105: revert the forced predicate to IsPasteUnsafe → the pre-paste
//     re-probe aborts on the banner, message reverts to queued → this fails.
func TestServe_ForceRateLimited_DeliversThroughRateLimitDefer(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "force-me-through", ForceRateLimited: true,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	prevProfile := tmuxio.ActivePaneProfile()
	tmuxio.SetActivePaneProfile(tmuxio.PaneProfile{
		PromptSentinel:   tmuxio.CodexPromptSentinel,
		RateLimitPattern: `Rate limited`,
	})
	t.Cleanup(func() { tmuxio.SetActivePaneProfile(prevProfile) })
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	var mu sync.Mutex
	var pasted string // empty until the paste lands
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			if strings.Contains(strings.Join(args, " "), "pane_in_mode") {
				return []byte("0"), nil // not in copy-mode
			}
			return []byte("0/0"), nil // cursor row/col
		case "load-buffer":
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				mu.Lock()
				pasted = string(b)
				mu.Unlock()
			}
			return nil, nil
		case "capture-pane":
			mu.Lock()
			defer mu.Unlock()
			if pasted == "" {
				return []byte("Rate limited"), nil // pre-paste: gate + #105 see rate-limited
			}
			return []byte(pasted), nil // post-paste: echo so delivery verifies
		default:
			return nil, nil
		}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	m := metrics.New()
	opts := fastOpts("bob")
	opts.GateDisabled = false           // enable the rate-limit gate (force site 1)
	opts.PrePasteSafetyDisabled = false // enable the #105 re-probe (force site 2)
	opts.Metrics = m
	opts.ObserveGateOpts = tmuxio.ObserveGateOpts{
		PollIntervalMin: time.Millisecond,
		PollIntervalMax: time.Millisecond,
		MaxWait:         15 * time.Millisecond,
	}

	stop, wait, logbuf := runServeInBackground(t, s, opts)
	waitFor(t, 2*time.Second, func() bool {
		d, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
		return len(d) == 1
	}, "expected the --force-rate-limited message to deliver through the rate-limit defer")
	stop()
	wait()

	if !strings.Contains(logbuf.String(), "gate_forced_ratelimited") {
		t.Errorf("expected gate_forced_ratelimited log (force bypass fired); got:\n%s", logbuf.String())
	}
}

// TestServe_ForceRateLimited_DoesNotBypassCopyMode is the safety pin: force is
// narrow. A --force-rate-limited message to a pane scrolled into copy-mode must
// STILL defer and never paste — copy-mode is content-corrupting (the 83b3 bug),
// not a rate-limit throttle, so the force flag does not reach it. Mirrors #526's
// TestServe_CopyMode_DefersRevertsNeverPastes with force=true; the load-bearing
// assertion is the negative one (zero paste primitives).
func TestServe_ForceRateLimited_DoesNotBypassCopyMode(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "display-message" && strings.Contains(args[len(args)-1], "pane_in_mode") {
			return []byte("1\n"), nil // scrolled up the whole test
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	r, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "force-must-not-paste-into-scroll",
		ForceRateLimited: true, // force is set — must still NOT punch through copy-mode
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	m := metrics.New()
	opts := fastOpts("bob")
	opts.GateDisabled = false
	opts.Metrics = m
	opts.ObserveGateOpts = tmuxio.ObserveGateOpts{
		PollIntervalMin: time.Millisecond,
		PollIntervalMax: time.Millisecond,
		MaxWait:         15 * time.Millisecond,
	}

	stop, wait, logbuf := runServeInBackground(t, s, opts)
	waitFor(t, 2*time.Second, func() bool {
		return gatherCounter(t, m, "tmux_tell_copymode_defer_total",
			map[string]string{"agent": "bob"}) >= 1
	}, "expected copy-mode deferral even with --force-rate-limited")
	stop()
	wait()

	final, gerr := s.GetMessage(ctx, r.PublicID)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if final.State == store.StateDelivered {
		t.Errorf("forced message delivered into copy-mode — force must NOT bypass paste-unsafe; calls=%v", calls)
	}
	if !strings.Contains(logbuf.String(), "gate_copymode_persist") {
		t.Errorf("expected gate_copymode_persist log (copy-mode still defers under force); got:\n%s", logbuf.String())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if strings.HasPrefix(c, "send-keys") ||
			strings.HasPrefix(c, "load-buffer") ||
			strings.HasPrefix(c, "paste-buffer") {
			t.Errorf("paste primitive fired into a scrolled pane despite force: %q — reproduces 83b3", c)
		}
	}
}
