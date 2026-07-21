package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// staleSessionWalker is a discover.Walker whose readers never carry a
// session-id, so LookupBySessionID(anything) returns "" — the session_stale
// condition (#783): the registered session-id resolves to no live pane.
func staleSessionWalker() *discover.Walker {
	return &discover.Walker{
		CmdlineReader:  func(int) (string, error) { return "bash\x00", nil },
		ChildrenReader: func(int) []int { return nil },
		EnvironReader:  func(int, string) (string, bool) { return "", false },
		MaxDepth:       1,
	}
}

// TestServe_SessionStale_ParksAfterThreshold is the #783 AC-1 test. The
// registered session-id resolves to no live pane (session_stale); name
// resolution falls back to a pane that classifies StateUnknown; #105's
// pre-paste-safety net refuses. Before #783 this retry-looped forever with no
// exit. Now the fast-path accrues a streak and, at SessionStaleThreshold, parks
// the mailman with StuckReasonSessionStale — the message stays queued (no loss),
// recoverable via `register --force`.
func TestServe_SessionStale_ParksAfterThreshold(t *testing.T) {
	prevIvl := setSessionStaleRetryIntervalForTest(2 * time.Millisecond)
	t.Cleanup(func() { setSessionStaleRetryIntervalForTest(prevIvl) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	// list-panes has the agent's pane, but the walker never resolves the
	// session-id → LookupBySessionID "" → session_stale.
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%3\t300\tbash\tbash\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	// The name-resolved pane classifies StateUnknown: a stable capture with no
	// prompt sentinel, cursor not on a sentinel row.
	var mu sync.Mutex
	probeCalls := 0
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			mu.Lock()
			probeCalls++
			mu.Unlock()
			return []byte("recent tool output\n$ \n"), nil // no ❯ sentinel → unknown
		case "display-message":
			last := args[len(args)-1]
			switch {
			case strings.Contains(last, "pane_in_mode"):
				return []byte("0\n"), nil
			case strings.Contains(last, "cursor"):
				return []byte("0/0\n"), nil // row 0 is not a sentinel row
			default:
				return []byte("bash\n"), nil
			}
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if err := s.SetSessionID(ctx, "bob", "STALE-uuid"); err != nil {
		t.Fatalf("set session-id: %v", err)
	}
	r, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "are you there?"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	opts := fastOpts("bob")
	opts.PrePasteSafetyDisabled = false
	opts.DriftCheckDisabled = false // session resolution runs only when drift-check is on
	opts.Walker = staleSessionWalker()
	opts.SessionStaleThreshold = 2 // park after 2 consecutive session_stale+unknown
	opts.StuckPollInterval = 5 * time.Millisecond

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	waitFor(t, 3*time.Second, func() bool {
		a, _ := s.GetAgent(ctx, "bob")
		return a.StuckReason != ""
	}, "agent did not park on session_stale")

	a, _ := s.GetAgent(ctx, "bob")
	if a.StuckReason != store.StuckReasonSessionStale {
		t.Fatalf("stuck_reason = %q, want %q", a.StuckReason, store.StuckReasonSessionStale)
	}
	// No data loss: the message reverted to queued, never delivered/failed.
	final, _ := s.GetMessage(ctx, r.PublicID)
	if final.State != store.StateQueued {
		t.Errorf("message state = %s, want queued (parked, retained)", final.State)
	}
	for _, want := range []string{"session_stale_stuck_backoff", "stuck"} {
		if !strings.Contains(logbuf.String(), want) {
			t.Errorf("log missing %q; got:\n%s", want, logbuf.String())
		}
	}
}

// TestServe_SessionStale_BenignIdleDoesNotPark is the safety pin: a session_stale
// whose name-resolved pane is a LIVE, classifiable session (probes idle) must NOT
// accrue the park streak — it falls through to normal delivery. This is what
// keeps the exit condition from stealing #105's legitimate transient cases: only
// session_stale + STuck-unknown parks, not session_stale alone.
func TestServe_SessionStale_BenignIdleDoesNotPark(t *testing.T) {
	prevIvl := setSessionStaleRetryIntervalForTest(2 * time.Millisecond)
	t.Cleanup(func() { setSessionStaleRetryIntervalForTest(prevIvl) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%3\t300\tclaude\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	// Stateful runner: idle Claude pane (❯ + cursor at sentinel) for the probe +
	// gate; after the paste lands it echoes the body so the verify token surfaces
	// and the message delivers. session_stale is still true (env carries no
	// session-id), but the pane is deliverable → no park.
	var mu sync.Mutex
	var body string
	pasted := false
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "load-buffer":
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				mu.Lock()
				body = string(b)
				mu.Unlock()
			}
		case "paste-buffer":
			mu.Lock()
			pasted = true
			mu.Unlock()
		case "capture-pane":
			mu.Lock()
			defer mu.Unlock()
			if pasted {
				return []byte(body), nil // post-paste: verify token present
			}
			return []byte("❯ \n"), nil // idle: ❯ + NBSP, empty composer
		case "display-message":
			last := args[len(args)-1]
			switch {
			case strings.Contains(last, "pane_in_mode"):
				return []byte("0\n"), nil
			case strings.Contains(last, "cursor"):
				return []byte("2/0\n"), nil // cursor at the sentinel column on the ❯ row → idle
			default:
				return []byte("node\n"), nil // live adapter (passes the #761 paste-capability gate)
			}
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	// The drift guard must find the pane hosting "bob".
	walker := staleSessionWalker()
	walker.CmdlineReader = func(pid int) (string, error) {
		if pid == 300 {
			return "claude\x00--resume\x00bob\x00", nil
		}
		return "bash\x00", nil
	}

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if err := s.SetSessionID(ctx, "bob", "STALE-uuid"); err != nil {
		t.Fatalf("set session-id: %v", err)
	}
	r, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "benign hello"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	opts := fastOpts("bob")
	opts.PrePasteSafetyDisabled = false
	opts.DriftCheckDisabled = false
	opts.Walker = walker
	opts.SessionStaleThreshold = 2
	opts.StuckPollInterval = 5 * time.Millisecond

	stop, wait, _ := runServeInBackgroundOpts(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	// The message drains (delivered) and the agent NEVER parks with session-stale.
	waitFor(t, 3*time.Second, func() bool {
		m, _ := s.GetMessage(ctx, r.PublicID)
		return m.State != store.StateQueued
	}, "benign session_stale message never left the queue")

	a, _ := s.GetAgent(ctx, "bob")
	if a.StuckReason == store.StuckReasonSessionStale {
		t.Fatalf("agent parked with session-stale on a benign (deliverable) session_stale pane — the streak must only accrue on StateUnknown")
	}
}

// TestServe_LiveSessionUnknown_DoesNotPark pins the guard that actually
// protects #105 (Surveyor #792 review): the `if sessionStale` gate. A live,
// RESOLVING session-id is NOT session_stale, so even a PERSISTENTLY-unknown pane
// must never enter the accrual path — it stays on the bare #105 revert-and-retry
// (the transient-popup case #105 is designed to wait out). The two sibling tests
// both set a stale session-id, so they pin the StateUnknown refinement WITHIN
// session_stale; only this one varies session_stale to FALSE, so it is the test
// that reds if the `sessionStale` condition is dropped (bare unknown → parks →
// #105's case stolen). Without it, that mutation passes green — the exact
// two-guards-one-pinned gap.
func TestServe_LiveSessionUnknown_DoesNotPark(t *testing.T) {
	prevIvl := setSessionStaleRetryIntervalForTest(2 * time.Millisecond)
	t.Cleanup(func() { setSessionStaleRetryIntervalForTest(prevIvl) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	// The session-id RESOLVES to the agent's pane (pid 300 carries it) → the
	// resolution block takes the sidPane!="" branch → sessionResolved=true,
	// sessionStale STAYS false.
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%3\t300\tclaude\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })
	walker := &discover.Walker{
		CmdlineReader:  func(int) (string, error) { return "claude\x00", nil },
		ChildrenReader: func(int) []int { return nil },
		EnvironReader: func(pid int, key string) (string, bool) {
			if key == discover.NeutralSessionIDEnv && pid == 300 {
				return "LIVE-uuid", true
			}
			return "", false
		},
		MaxDepth: 1,
	}

	// The pane classifies StateUnknown on every probe (no prompt sentinel), so the
	// #105 pre-paste-safety net refuses every delivery — but this must NOT park.
	var mu sync.Mutex
	probeCalls := 0
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			mu.Lock()
			probeCalls++
			mu.Unlock()
			return []byte("recent tool output\n$ \n"), nil // no ❯ sentinel → unknown
		case "display-message":
			last := args[len(args)-1]
			switch {
			case strings.Contains(last, "pane_in_mode"):
				return []byte("0\n"), nil
			case strings.Contains(last, "cursor"):
				return []byte("0/0\n"), nil
			default:
				return []byte("bash\n"), nil
			}
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if err := s.SetSessionID(ctx, "bob", "LIVE-uuid"); err != nil {
		t.Fatalf("set session-id: %v", err)
	}
	_, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "live but unknown"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	opts := fastOpts("bob") // GateDisabled=true → the #105 re-probe loop runs fast
	opts.PrePasteSafetyDisabled = false
	opts.DriftCheckDisabled = false // session resolution runs; sessionResolved=true skips the name drift-check
	opts.Walker = walker
	opts.SessionStaleThreshold = 2 // a mutated build (dropping the sessionStale gate) would park after 2
	opts.StuckPollInterval = 5 * time.Millisecond

	stop, wait, _ := runServeInBackgroundOpts(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	// Let the #105 loop run far past the threshold. A build that dropped the
	// `sessionStale` gate would park within ~2 iterations (a few ms); the correct
	// build never parks because session_stale is false and the fast-path is skipped.
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	pc := probeCalls
	mu.Unlock()
	if pc < 3 {
		t.Fatalf("only %d capture probes — the loop did not exercise the unknown-abort path enough for the assertion to be meaningful", pc)
	}
	a, _ := s.GetAgent(ctx, "bob")
	if a.StuckReason != "" {
		t.Fatalf("agent parked (%q) on a LIVE (non-session_stale) unknown pane — the `sessionStale` gate must fence off accrual so #105's transient-unknown case is never stolen", a.StuckReason)
	}
}

// TestRecipientStatus_DisclosesStuckReason is the #783 AC-2 test: a parked
// recipient (mailman up but stuck_reason set) is disclosed in RecipientStatus so
// a sender whose message sits queued can tell stuck-invisibly from merely-slow.
func TestRecipientStatus_DisclosesStuckReason(t *testing.T) {
	prevList := tmuxio.SetListPanesRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%3\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesRunner(prevList) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if err := s.SetStuck(ctx, "bob", store.StuckReasonSessionStale); err != nil {
		t.Fatalf("set stuck: %v", err)
	}

	rs, err := resolveRecipientStatus(ctx, s, "bob")
	if err != nil {
		t.Fatalf("resolveRecipientStatus: %v", err)
	}
	if rs.StuckReason != store.StuckReasonSessionStale {
		t.Errorf("RecipientStatus.StuckReason = %q, want %q", rs.StuckReason, store.StuckReasonSessionStale)
	}
	line := recipientOneLine(rs)
	if !strings.Contains(line, "PARKED") || !strings.Contains(line, store.StuckReasonSessionStale) {
		t.Errorf("recipientOneLine = %q, want it to disclose the PARKED session-stale state", line)
	}

	// Cleared recipient discloses nothing.
	if err := s.ClearStuck(ctx, "bob"); err != nil {
		t.Fatalf("clear stuck: %v", err)
	}
	rs2, _ := resolveRecipientStatus(ctx, s, "bob")
	if rs2.StuckReason != "" {
		t.Errorf("after clear, StuckReason = %q, want empty", rs2.StuckReason)
	}
}
