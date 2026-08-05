package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// fastOpts gives a mailman that doesn't sleep meaningfully — tests must
// finish in milliseconds, not seconds.
func fastOpts(agent string) serveOpts {
	return serveOpts{
		Agent:              agent,
		InterMessageDelay:  time.Millisecond,
		IdlePollInterval:   time.Millisecond,
		PauseCheckInterval: time.Millisecond,
		DeliverTimeout:     5 * time.Second,
		// Existing serve tests drive the fake runner with capture-pane
		// responses tuned for the delivery sequence only. Bypass the
		// observe-gate (#92) so they keep observing the same call
		// shape. New gate-specific tests live in observe_gate_test.go.
		GateDisabled: true,
		// Same idea for the silent-drift guard (#37): existing tests
		// don't fake ListPanesWithPID or /proc readers, so leave the
		// check off here. Drift-specific tests opt in by setting
		// DriftCheckDisabled=false and injecting a Walker.
		DriftCheckDisabled: true,
		// Same for the pre-paste safety check (#105 Half 2): existing
		// tests don't fake AgentState classifications, so the safety
		// check would see the runner's body-echoed pane content and
		// classify as Unknown → abort every delivery. Safety-check-
		// specific tests opt in by setting PrePasteSafetyDisabled=false
		// and faking AgentState.
		PrePasteSafetyDisabled: true,
		// Same for the #448 provider-cap: the self-probe calls tmuxio.AgentState
		// (which these tests don't fake) and the gate counts cross-mailman state.
		// Off here; cap-specific tests opt in by setting ProviderCapDisabled=false.
		ProviderCapDisabled: true,
		// #449: collapse the 5s post-deliver cooldown to a tick so integration
		// tests that drive several deliveries don't pay it. Cooldown-specific
		// tests set it explicitly.
		PostDeliverCooldown: time.Millisecond,
	}
}

// withSuccessfulDelivery installs a fake tmuxRunner that captures the body
// passed via load-buffer and replays it on capture-pane, so the verify
// token (the message's "id <public_id>") is found on the first attempt.
//
// Also collapses the package-level settle delay to a microsecond so
// integration tests don't pay 500ms per delivery — the settle delay is
// a real-keyboard timing concern, not a state-machine property to pin.
func withSuccessfulDelivery(t *testing.T) {
	t.Helper()
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })

	var mu sync.Mutex
	var lastBody string
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if args[0] == "load-buffer" && stdin != nil {
			b, _ := io.ReadAll(stdin)
			mu.Lock()
			lastBody = string(b)
			mu.Unlock()
		}
		if args[0] == "capture-pane" {
			mu.Lock()
			defer mu.Unlock()
			return []byte(lastBody), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
}

// syncBuffer is a goroutine-safe bytes.Buffer for the background-serve test
// helpers below: the mailman goroutine writes log lines via log.Logger while
// the test polls String() mid-run (e.g. waitFor). A plain *bytes.Buffer raced
// (logger.Write vs String()) — invisible to CI (no -race) but red on the
// pre-push -race gate for every contributor (#637). Write + String both lock.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func runServeInBackground(t *testing.T, s *store.Store, opts serveOpts) (cancel func(), wait func() int, logbuf *syncBuffer) {
	t.Helper()
	stopCtx, stop := context.WithCancel(context.Background())
	logbuf = &syncBuffer{}
	logger := log.New(logbuf, "[mailman/test] ", 0)
	var (
		exit int
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		exit = runServeWithStore(stopCtx, s, opts, logger, io.Discard, io.Discard)
	}()
	return stop, func() int { wg.Wait(); return exit }, logbuf
}

// TestServe_ExitsCleanWhenAgentUnregistered pins #340: agent-not-found is
// substrate-permanent for this unit instance, so serve exits with status 0
// (success — systemd's Restart=on-failure ignores it) instead of 69
// (UNAVAILABLE, which restart-looped under enough orphan units and triggered
// the alcatraz-infra#39 DB-contention freeze). The log line must still tell
// the operator how to recover (register or discover, then restart the unit).
func TestServe_ExitsCleanWhenAgentUnregistered(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stderr bytes.Buffer
	exit := runServeWithStore(context.Background(), s, fastOpts("ghost"),
		log.New(&stderr, "", 0), io.Discard, &stderr)
	if exit != exitOK {
		t.Errorf("exit = %d, want %d (exitOK: agent-not-found is "+
			"substrate-permanent; systemd should record success and stop "+
			"restart-looping per #340)", exit, exitOK)
	}
	if !strings.Contains(stderr.String(), "not registered in DB") {
		t.Errorf("stderr missing operator-recovery hint: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "restart-loop") {
		t.Errorf("stderr missing #340 framing: %q", stderr.String())
	}
}

// TestServe_ExitsCleanWhenPaneEmpty is the sibling check for the no-pane_id
// branch: same shape (substrate-permanent for THIS instance), same fix.
func TestServe_ExitsCleanWhenPaneEmpty(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "")

	var stderr bytes.Buffer
	exit := runServeWithStore(ctx, s, fastOpts("bob"),
		log.New(&stderr, "", 0), io.Discard, &stderr)
	if exit != exitOK {
		t.Errorf("exit = %d, want %d (exitOK per #340)", exit, exitOK)
	}
	if !strings.Contains(stderr.String(), "no pane_id") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestServe_DeliversInFIFOOrder(t *testing.T) {
	withSuccessfulDelivery(t)

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	for i := 0; i < 4; i++ {
		_, _ = s.InsertMessage(ctx, store.InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "msg",
		})
	}

	stop, wait, _ := runServeInBackground(t, s, fastOpts("bob"))
	// Poll briefly until all 4 are delivered.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(all) == 4 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	delivered, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "bob", State: store.StateDelivered, Limit: 10,
	})
	if len(delivered) != 4 {
		t.Fatalf("delivered = %d, want 4", len(delivered))
	}
	// FIFO: ids ascending.
	for i := 1; i < len(delivered); i++ {
		if delivered[i-1].ID >= delivered[i].ID {
			t.Errorf("FIFO violation at %d: %d >= %d",
				i, delivered[i-1].ID, delivered[i].ID)
		}
	}
}

func TestServe_RespectsPaused(t *testing.T) {
	withSuccessfulDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_ = s.SetPaused(ctx, "bob", true)

	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "queued"})

	stop, wait, _ := runServeInBackground(t, s, fastOpts("bob"))
	time.Sleep(50 * time.Millisecond)

	delivered, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "bob", State: store.StateDelivered, Limit: 10,
	})
	if len(delivered) != 0 {
		t.Errorf("delivered while paused = %d, want 0", len(delivered))
	}

	// Resume; expect delivery shortly.
	_ = s.SetPaused(ctx, "bob", false)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
		if len(d) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()
	final, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
	if len(final) != 1 {
		t.Errorf("after resume = %d, want 1", len(final))
	}
}

func TestServe_RecoversDeliveringOnStart(t *testing.T) {
	withSuccessfulDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	// Two queued, claim both → they're stuck in delivering (simulated crash).
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "2"})
	_, _ = s.ClaimNext(ctx, "bob")
	_, _ = s.ClaimNext(ctx, "bob")

	stop, wait, logbuf := runServeInBackground(t, s, fastOpts("bob"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
		if len(d) == 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	if !strings.Contains(logbuf.String(), "recovered count=2") {
		t.Errorf("expected recovery log; got:\n%s", logbuf.String())
	}
	d, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
	if len(d) != 2 {
		t.Errorf("delivered = %d, want 2", len(d))
	}
}

// TestServe_RecoversDeliveringBeforeHookContextShortCircuit pins #357: a stale
// `delivering` row is recovered even when serve exits early via the
// hook-context short-circuit (delivery_mode=hook-context means the mailman does
// not paste, so it exits immediately — but RecoverDelivering must still fire
// before that exit to unblock orphaned rows left by a prior paste-and-enter run).
func TestServe_RecoversDeliveringBeforeHookContextShortCircuit(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	// Seed a message and simulate a crash: claim it (→ delivering) but never mark.
	r, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "orphan"})
	_, _ = s.ClaimNext(ctx, "bob")
	// Transition bob to hook-context — the next serve startup will short-circuit.
	if err := s.SetDeliveryMode(ctx, "bob", store.DeliveryModeHookContext); err != nil {
		t.Fatalf("set mode: %v", err)
	}

	var logbuf bytes.Buffer
	logger := log.New(&logbuf, "[mailman/test] ", 0)
	exit := runServeWithStore(context.Background(), s, fastOpts("bob"), logger, &bytes.Buffer{}, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d, want exitOK", exit)
	}
	// Short-circuit must have fired.
	if !strings.Contains(logbuf.String(), "delivery_mode=hook-context") {
		t.Errorf("expected hook-context short-circuit log; got:\n%s", logbuf.String())
	}
	// RecoverDelivering must have fired BEFORE the short-circuit.
	if !strings.Contains(logbuf.String(), "recovered count=1") {
		t.Errorf("expected recovery log before short-circuit; got:\n%s", logbuf.String())
	}
	m, _ := s.GetMessage(ctx, r.PublicID)
	if m.State != store.StateQueued {
		t.Errorf("orphan message state = %s, want queued (recovered)", m.State)
	}
}

// TestServe_RecoversDeliveringBeforeMailboxOnlyShortCircuit is the same pin for
// the mailbox-only short-circuit path (#357). Unlike hook-context, mailbox-only
// has NO other automatic recovery path — RecoverDelivering at serve startup is
// the only mechanism.
func TestServe_RecoversDeliveringBeforeMailboxOnlyShortCircuit(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	r, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "orphan"})
	_, _ = s.ClaimNext(ctx, "bob")
	if err := s.SetDeliveryMode(ctx, "bob", store.DeliveryModeMailboxOnly); err != nil {
		t.Fatalf("set mode: %v", err)
	}

	var logbuf bytes.Buffer
	logger := log.New(&logbuf, "[mailman/test] ", 0)
	exit := runServeWithStore(context.Background(), s, fastOpts("bob"), logger, &bytes.Buffer{}, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d, want exitOK", exit)
	}
	if !strings.Contains(logbuf.String(), "delivery_mode=mailbox-only") {
		t.Errorf("expected mailbox-only short-circuit log; got:\n%s", logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "recovered count=1") {
		t.Errorf("expected recovery log before short-circuit; got:\n%s", logbuf.String())
	}
	m, _ := s.GetMessage(ctx, r.PublicID)
	if m.State != store.StateQueued {
		t.Errorf("orphan message state = %s, want queued (recovered)", m.State)
	}
}

func TestServe_MarksFailedOnDeliveryError(t *testing.T) {
	// Fake runner: load-buffer fails. Deliver returns an error.
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] == "load-buffer" {
			return []byte("nope"), &errString{"load-buffer failed"}
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "x"})

	stop, wait, _ := runServeInBackground(t, s, fastOpts("bob"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateFailed, Limit: 10})
		if len(f) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	failed, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateFailed, Limit: 10})
	if len(failed) != 1 {
		t.Fatalf("failed rows = %d, want 1", len(failed))
	}
	if !failed[0].Error.Valid || !strings.Contains(failed[0].Error.String, "load-buffer") {
		t.Errorf("error = %v, want mention of load-buffer", failed[0].Error)
	}
}

type errString struct{ s string }

func (e *errString) Error() string { return e.s }

func TestIsCantFindPaneError(t *testing.T) {
	cases := map[string]bool{
		"":                 false,
		"some other error": false,
		"tmuxio: paste-buffer: can't find pane: %7": true,
		"can't find pane: %42":                      true,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			var err error
			if in != "" {
				err = &errString{in}
			}
			if got := isCantFindPaneError(err); got != want {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

func TestServe_AutoHealOnPaneDrift(t *testing.T) {
	// Sets up: stored pane is %7 (stale); LookupByName returns %9 (current).
	// Deliver fails on %7 ("can't find pane"), succeeds on %9.
	var captures atomic.Int64
	var (
		bodyMu sync.Mutex
		body   string
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
			// First call targets %7 (stale) → fail.
			// Second call targets %9 (current) → succeed.
			for i, a := range args {
				if a == "-t" && i+1 < len(args) && args[i+1] == "%7" {
					return []byte("can't find pane: %7"), &errString{"exit 1: can't find pane: %7"}
				}
			}
			return nil, nil
		case "send-keys":
			return nil, nil
		case "capture-pane":
			captures.Add(1)
			bodyMu.Lock()
			defer bodyMu.Unlock()
			return []byte(body), nil
		case "delete-buffer":
			return nil, nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bosun", "%7") // ← stale
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bosun", Body: "auto-heal me",
	})

	// Walker that knows bosun is now at %9.
	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			if pid == 999 {
				return "claude\x00--resume\x00bosun\x00", nil
			}
			return "", nil
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%9\t999\tbosun\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	opts := fastOpts("bosun")
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bosun", State: store.StateDelivered, Limit: 10,
		})
		if len(d) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	// Message delivered.
	delivered, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "bosun", State: store.StateDelivered, Limit: 10,
	})
	if len(delivered) != 1 {
		t.Errorf("delivered = %d, want 1; log:\n%s", len(delivered), logbuf.String())
	}
	// Row was healed.
	a, _ := s.GetAgent(ctx, "bosun")
	if a.PaneID != "%9" {
		t.Errorf("pane_id after heal = %s, want %%9", a.PaneID)
	}
	// auto_heal log line emitted.
	if !strings.Contains(logbuf.String(), "auto_heal") {
		t.Errorf("expected auto_heal log line; got:\n%s", logbuf.String())
	}
}

// TestServe_InputRaced_RevertsToQueued pins the #616 serve-loop wiring: when
// Deliver's tightest-window pre-paste check finds operator content in the input
// row and returns ErrInputRaced, the mailman must NOT mark the message delivered
// or failed — it reverts to queued (RecoverDelivering) and retries on a later
// cycle, leaving the operator's draft untouched and unpasted. fastOpts disables
// the serve-level pre-paste probe, so the ONLY thing that can abort here is the
// Deliver-level race check — isolating the sentinel→revert mapping. (The tmuxio
// test pins the abort-before-paste; this pins what serve does with the sentinel.)
func TestServe_InputRaced_RevertsToQueued(t *testing.T) {
	prevProfile := tmuxio.ActivePaneProfile()
	tmuxio.SetActivePaneProfile(tmuxio.ClaudePaneProfile())
	t.Cleanup(func() { tmuxio.SetActivePaneProfile(prevProfile) })

	var pastes atomic.Int64
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			return []byte("a prior turn\n" + tmuxio.PromptSentinel + "operator mid-typing"), nil
		case "display-message":
			return []byte("18/1"), nil // cursor past the sentinel col → operator content
		case "paste-buffer":
			pastes.Add(1)
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "engineer", "%3")
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "engineer", Body: "do not prepend me",
	})

	opts := fastOpts("engineer")
	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logbuf.String(), "input_raced") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	if !strings.Contains(logbuf.String(), "input_raced") {
		t.Fatalf("expected input_raced log line; got:\n%s", logbuf.String())
	}
	// Never pasted onto the operator's draft.
	if n := pastes.Load(); n != 0 {
		t.Errorf("paste-buffer ran %d times; a raced input must abort before any paste", n)
	}
	// Not terminally delivered or failed — reverted to queued for retry.
	delivered, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "engineer", State: store.StateDelivered, Limit: 10})
	if len(delivered) != 0 {
		t.Errorf("delivered = %d, want 0 (a raced input must not mark delivered)", len(delivered))
	}
	failed, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "engineer", State: store.StateFailed, Limit: 10})
	if len(failed) != 0 {
		t.Errorf("failed = %d, want 0 (a raced input must not mark failed)", len(failed))
	}
}

func TestServe_AutoHealNoMatchStillFails(t *testing.T) {
	// Deliver fails with can't-find-pane; LookupByName returns no match;
	// message ends in 'failed'.
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] == "paste-buffer" {
			return []byte("can't find pane: %7"), &errString{"can't find pane: %7"}
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte(""), nil // no panes
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "ghost", "%7")
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "ghost", Body: "no rebind possible",
	})

	opts := fastOpts("ghost")
	opts.Walker = discover.New()

	stop, wait, _ := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "ghost", State: store.StateFailed, Limit: 10,
		})
		if len(f) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	failed, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "ghost", State: store.StateFailed, Limit: 10,
	})
	if len(failed) != 1 {
		t.Errorf("failed = %d, want 1", len(failed))
	}
}

// staleFlushTmuxMock is a shared tmux runner factory for the stale-flush test
// family. It simulates a gate that fires Stale=true after 2 iterations (6
// capture-pane calls total). The caller supplies n7fn, which controls what the
// fresh ExtractInputContent call at position n=7 returns (and may be an error).
// n≥8 are delivery-verify captures that echo back the last load-buffer body.
//
// Pane layout (0-indexed rows):
//
//	row 0: "header"
//	row 1: "❯ <content>" — sentinel row
//	         cursor_y=1, cursor_x=3 > sentinelCol=2 → StateAwaitingOperator
//	row 2: 60× ─ — isInputAreaBoundary stops extractInputContent walk here
//	row 3: "  ⏵⏵ status"
func staleFlushTmuxMock(
	staleContent string,
	n7fn func() ([]byte, error),
) (runner func(context.Context, io.Reader, ...string) ([]byte, error), lastBody func() string) {
	const separatorRow = "────────────────────────────────────────────────────────────"
	makePane := func(c string) string {
		return "header\n" + tmuxio.PromptSentinel + c + "\n" + separatorRow + "\n  ⏵⏵ status\n"
	}
	capAB := makePane(staleContent)

	var (
		captureN atomic.Int32
		mu       sync.Mutex
		body     string
	)
	r := func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			if strings.Contains(args[len(args)-1], "pane_in_mode") {
				return []byte("0\n"), nil
			}
			return []byte("3/1\n"), nil
		case "capture-pane":
			n := captureN.Add(1)
			if n == 7 {
				return n7fn()
			}
			if n >= 8 {
				mu.Lock()
				b := body
				mu.Unlock()
				return []byte(b), nil
			}
			return []byte(capAB), nil
		case "load-buffer":
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				mu.Lock()
				body = string(b)
				mu.Unlock()
			}
			return nil, nil
		case "paste-buffer", "send-keys", "delete-buffer":
			return nil, nil
		}
		return nil, nil
	}
	return r, func() string { mu.Lock(); defer mu.Unlock(); return body }
}

// staleFlushServeOpts returns the standard serveOpts used by the stale-flush
// test family. The gate is configured to fire Stale=true after 2 iterations
// (≈ 5ms each with a 3ms threshold).
func staleFlushServeOpts() serveOpts {
	return serveOpts{
		Agent:                  "bob",
		InterMessageDelay:      time.Millisecond,
		IdlePollInterval:       time.Millisecond,
		PauseCheckInterval:     time.Millisecond,
		DeliverTimeout:         5 * time.Second,
		PostDeliverCooldown:    time.Millisecond,
		DriftCheckDisabled:     true,
		PrePasteSafetyDisabled: true,
		ProviderCapDisabled:    true,
		NotifyEmojiDisabled:    true,
		GateDisabled:           false,
		ObserveGateOpts: tmuxio.ObserveGateOpts{
			InputStaleThreshold: 3 * time.Millisecond,
			PollIntervalMin:     5 * time.Millisecond,
			PollIntervalMax:     5 * time.Millisecond,
			MaxWait:             5 * time.Second,
		},
	}
}

// TestServe_StaleFlushArchivesFreshContent pins the #879 fix: when the
// observe-gate fires Stale=true and the fresh ExtractInputContent returns
// non-empty content, serve.go must archive the FRESH capture rather than
// outcome.InputContent (captured at stale-detection time). The operator
// may have continued typing in the window between gate detection and flush.
//
// Mutation test: remove the `archiveContent = fresh` assignment in serve.go's
// stale-flush block so the stale snapshot is always used. The test fails:
// archived content equals staleContent instead of freshContent.
func TestServe_StaleFlushArchivesFreshContent(t *testing.T) {
	const (
		staleContent = "stale draft line"
		freshContent = "stale draft line MORE TYPED AFTER DETECTION"
	)

	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })

	const separatorRow = "────────────────────────────────────────────────────────────"
	freshPane := "header\n" + tmuxio.PromptSentinel + freshContent + "\n" + separatorRow + "\n  ⏵⏵ status\n"
	runner, _ := staleFlushTmuxMock(staleContent, func() ([]byte, error) {
		return []byte(freshPane), nil
	})
	prev := tmuxio.SetTmuxRunner(runner)
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%5")
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello from alice",
	})

	stop, wait, _ := runServeInBackground(t, s, staleFlushServeOpts())

	deadline := time.Now().Add(5 * time.Second)
	var strandedID string
	for time.Now().Before(deadline) {
		drafts, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", Kind: store.KindStrandedDraft, Limit: 10,
		})
		if len(drafts) >= 1 {
			strandedID = drafts[0].PublicID
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	if strandedID == "" {
		t.Fatal("no stranded_draft row written — gate stale-flush or archive did not fire")
	}
	m, err := s.GetMessage(ctx, strandedID)
	if err != nil {
		t.Fatalf("GetMessage(%s): %v", strandedID, err)
	}
	_, _, got, ok := parseStrandedBody(m.Body)
	if !ok {
		t.Fatalf("parseStrandedBody failed for body:\n%s", m.Body)
	}
	if got != freshContent {
		t.Errorf("archived content = %q\nwant fresh content = %q\n(stale content was %q)",
			got, freshContent, staleContent)
	}
}

// TestServe_StaleFlushSkipsArchiveWhenBufferEmpty pins the semantic: if the
// fresh ExtractInputContent returns ("", nil) — meaning the operator cleared
// their draft between gate-detection and flush — serve.go must NOT archive
// the stale gate-snapshot. That would be data-resurrection: the operator
// deliberately removed the text, and the archive would put it back.
//
// Mutation test: change skipArchive to always be false in serve.go's
// stale-flush block. The test fails: a stranded_draft row is written (with
// the stale snapshot) when none should be.
func TestServe_StaleFlushSkipsArchiveWhenBufferEmpty(t *testing.T) {
	const staleContent = "stale draft line"

	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })

	const separatorRow = "────────────────────────────────────────────────────────────"
	// Empty pane: sentinel row has no content after the prompt, so
	// extractInputContent returns "".
	emptyPane := "header\n" + tmuxio.PromptSentinel + "\n" + separatorRow + "\n  ⏵⏵ status\n"
	runner, _ := staleFlushTmuxMock(staleContent, func() ([]byte, error) {
		return []byte(emptyPane), nil // ("", nil) — operator cleared buffer
	})
	prev := tmuxio.SetTmuxRunner(runner)
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%5")
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello from alice",
	})

	stop, wait, _ := runServeInBackground(t, s, staleFlushServeOpts())

	// Wait long enough that a stranded_draft WOULD appear if archiving fired,
	// but stop before the 5s deadline so the test doesn't hang on success.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		drafts, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", Kind: store.KindStrandedDraft, Limit: 10,
		})
		if len(drafts) >= 1 {
			stop()
			wait()
			t.Fatalf("stranded_draft row written when buffer was empty — data-resurrection: %+v", drafts[0])
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()
}

// TestServe_StaleFlushArchivesStaleOnExtractError pins the fallback: if the
// fresh ExtractInputContent returns an error, serve.go must fall back to the
// stale gate-snapshot (outcome.InputContent) rather than silently discarding
// any record of the operator's draft.
//
// Mutation test: change the fallback path so errors are treated like the empty
// case (skipArchive = true). The test fails: no stranded_draft row is written.
func TestServe_StaleFlushArchivesStaleOnExtractError(t *testing.T) {
	const staleContent = "stale draft line"

	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })

	runner, _ := staleFlushTmuxMock(staleContent, func() ([]byte, error) {
		return nil, errors.New("tmux: capture-pane: simulated failure") // ("", err)
	})
	prev := tmuxio.SetTmuxRunner(runner)
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%5")
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello from alice",
	})

	stop, wait, _ := runServeInBackground(t, s, staleFlushServeOpts())

	deadline := time.Now().Add(5 * time.Second)
	var strandedID string
	for time.Now().Before(deadline) {
		drafts, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", Kind: store.KindStrandedDraft, Limit: 10,
		})
		if len(drafts) >= 1 {
			strandedID = drafts[0].PublicID
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	if strandedID == "" {
		t.Fatal("no stranded_draft row written — extract error should fall back to stale snapshot")
	}
	m, err := s.GetMessage(ctx, strandedID)
	if err != nil {
		t.Fatalf("GetMessage(%s): %v", strandedID, err)
	}
	_, _, got, ok := parseStrandedBody(m.Body)
	if !ok {
		t.Fatalf("parseStrandedBody failed for body:\n%s", m.Body)
	}
	if got != staleContent {
		t.Errorf("archived content = %q\nwant stale content = %q (extract error should fall back to stale)",
			got, staleContent)
	}
}

// TestServe_StaleFlushSkipsArchiveForStrandedDraftDelivery pins the #906 fix:
// when the triggering message is itself a stranded-draft notification, the
// observe-gate's stale-flush block must NOT archive the pane content, because
// doing so would insert another stranded-draft notification — which would in
// turn trigger another archive on its delivery, forming a self-sustaining loop.
//
// Mutation test: remove the `msg.Kind != store.KindStrandedDraft` guard from
// serve.go's stale-flush condition. The test fails: a second stranded_draft row
// appears within the deadline, showing the loop fired.
func TestServe_StaleFlushSkipsArchiveForStrandedDraftDelivery(t *testing.T) {
	const staleContent = "stale draft line"

	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })

	// Pane holds staleContent throughout — so the gate fires Stale=true and
	// InputContent="stale draft line". Without the msg.Kind guard the archive
	// block would run: n=7 (n7fn) would be consumed by ExtractInputContent,
	// see staleContent, and insert a second stranded-draft row.
	//
	// With the guard: the archive block is skipped. n=7 is instead consumed by
	// deliverOne's pre-paste race check (capture-pane before load-buffer).
	// The stale pane makes inputRowCleared return anchored=true, cleared=false →
	// ErrInputRaced → delivery deferred. The mailman logs "input_raced …".
	//
	// That "input_raced" log line is the delivery-confirmation signal: it proves
	// mailman ran the full delivery pipeline (observe-gate → guard → deliverOne →
	// race check) and did not silently skip the message, which is what Surveyor's
	// "nothing observed" concern requires.
	runner, _ := staleFlushTmuxMock(staleContent, func() ([]byte, error) {
		const separatorRow = "────────────────────────────────────────────────────────────"
		return []byte("header\n" + tmuxio.PromptSentinel + staleContent + "\n" + separatorRow + "\n  ⏵⏵ status\n"), nil
	})
	prev := tmuxio.SetTmuxRunner(runner)
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%5")

	// Insert a stranded-draft notice from bob to bob (simulates the second
	// iteration of the loop: the first real delivery already archived, and
	// now this notification is waiting for delivery to bob).
	_, insertErr := s.InsertNotice(ctx, store.InsertParams{
		FromAgent: "bob",
		ToAgent:   "bob",
		Body:      renderStrandedDraftBody("%5", "trigger-000", staleContent),
		Kind:      store.KindStrandedDraft,
	})
	if insertErr != nil {
		t.Fatalf("InsertNotice: %v", insertErr)
	}

	stop, wait, logbuf := runServeInBackground(t, s, staleFlushServeOpts())

	// Poll: fail fast if the loop fires, exit as soon as delivery is confirmed.
	// Both conditions must be checked together — a test that only watches for
	// the loop can pass because delivery never happened (mailman didn't run, or
	// the guard accidentally suppressed delivery).
	//
	// Delivery signal: the "input_raced" log line (emitted by the ErrInputRaced
	// handler in serve.go) proves mailman ran the full delivery pipeline and
	// reached deliverOne — it cannot appear unless the observe-gate ran, the
	// guard fired (skipping the archive block), and deliverOne's race check
	// found the stale pane content. The race check fires at n=7 (before
	// load-buffer), so this signal lands at ~10ms — well within the 500ms
	// deadline without depending on verification completing.
	deadline := time.Now().Add(500 * time.Millisecond)
	var deliveryConfirmed bool
	for time.Now().Before(deadline) {
		drafts, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", Kind: store.KindStrandedDraft, Limit: 10,
		})
		if len(drafts) >= 2 {
			stop()
			wait()
			t.Fatalf("stranded-draft loop fired: %d rows for bob (want ≤ 1) — msg.Kind guard is missing", len(drafts))
		}
		if strings.Contains(logbuf.String(), "input_raced") {
			deliveryConfirmed = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()
	if !deliveryConfirmed {
		t.Fatal("mailman did not attempt stranded-draft delivery within 500ms (no input_raced log) — guard may have suppressed delivery or mailman did not run")
	}
}

// runServeInBackgroundOpts is like runServeInBackground but accepts a full
// serveOpts so tests can plug in a walker.
func runServeInBackgroundOpts(t *testing.T, s *store.Store, opts serveOpts) (cancel func(), wait func() int, logbuf *syncBuffer) {
	t.Helper()
	stopCtx, stop := context.WithCancel(context.Background())
	logbuf = &syncBuffer{}
	logger := log.New(logbuf, "[mailman/test] ", 0)
	var (
		exit int
		wg   sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		exit = runServeWithStore(stopCtx, s, opts, logger, io.Discard, io.Discard)
	}()
	return stop, func() int { wg.Wait(); return exit }, logbuf
}
