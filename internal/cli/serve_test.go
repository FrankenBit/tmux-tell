package cli

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
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

func runServeInBackground(t *testing.T, s *store.Store, opts serveOpts) (cancel func(), wait func() int, logbuf *bytes.Buffer) {
	t.Helper()
	stopCtx, stop := context.WithCancel(context.Background())
	logbuf = &bytes.Buffer{}
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

// fakeWalker stubs discover.Walker.LookupByName for the auto-heal tests.
type fakeWalker struct {
	hits map[string]string // agent → pane id
}

func (f *fakeWalker) walker() *discover.Walker {
	return &discover.Walker{
		CmdlineReader:  func(int) (string, error) { return "", nil },
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       0,
	}
}

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

// runServeInBackgroundOpts is like runServeInBackground but accepts a full
// serveOpts so tests can plug in a walker.
func runServeInBackgroundOpts(t *testing.T, s *store.Store, opts serveOpts) (cancel func(), wait func() int, logbuf *bytes.Buffer) {
	t.Helper()
	stopCtx, stop := context.WithCancel(context.Background())
	logbuf = &bytes.Buffer{}
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
