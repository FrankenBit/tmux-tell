package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestRunTrackWatch_ExitsOnTerminalState verifies the polling loop
// emits the initial state and exits cleanly when the message reaches
// a terminal state (delivered or failed). Per #49.
func TestRunTrackWatch_ExitsOnTerminalState(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Pre-stage the message as delivered so the first poll exits.
	if _, err := s.ClaimNext(ctx, "bob"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.MarkDelivered(ctx, res.PublicID); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exit := runTrackWatch(s, res.PublicID, "text",
		10*time.Millisecond, 0, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "STATE\tdelivered") {
		t.Errorf("expected delivered state in output; got %s", stdout.String())
	}
}

// TestRunTrackWatch_RendersOnEveryStateChange — start in queued, poll
// observes the same state once, then a background goroutine transitions
// to delivered. The watch loop should render twice (queued + delivered)
// and exit.
func TestRunTrackWatch_RendersOnEveryStateChange(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Transition to delivered after a short delay so the watch loop
	// observes queued first, then delivered.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = s.ClaimNext(ctx, "bob")
		_ = s.MarkDelivered(ctx, res.PublicID)
	}()

	var stdout, stderr bytes.Buffer
	exit := runTrackWatch(s, res.PublicID, "text",
		5*time.Millisecond, 5*time.Second, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	out := stdout.String()
	if !strings.Contains(out, "STATE\tqueued") {
		t.Errorf("expected queued state in output; got %s", out)
	}
	if !strings.Contains(out, "STATE\tdelivered") {
		t.Errorf("expected delivered state in output; got %s", out)
	}
}

// TestRunTrackWatch_TimeoutExits — message stays queued forever; the
// watch loop should exit on timeout without an error code.
//
// #281: the old assertion was a brittle wall-clock CEILING (`elapsed > 200ms`)
// that flaked under `-race` + concurrent load — a 30ms context timeout routinely
// measures well past 200ms once the race detector and parallel tests contend the
// scheduler, even though the loop exits correctly. The message never leaves
// `queued` (never claimed, never delivered), so the terminal-state branch is
// unreachable and `exit == exitOK` is itself the proof the timeout path was
// taken. The timing checks below are anchored on `context.WithTimeout`'s
// deadline semantics — a deterministic FLOOR plus a generous, jitter-immune
// ceiling — rather than a tight wall-clock bound the host load can blow past.
func TestRunTrackWatch_TimeoutExits(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	const (
		interval = 5 * time.Millisecond
		timeout  = 30 * time.Millisecond
	)

	var stdout, stderr bytes.Buffer
	start := time.Now()
	exit := runTrackWatch(s, res.PublicID, "text", interval, timeout, &stdout, &stderr)
	elapsed := time.Since(start)

	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	// Deterministic floor: the watch must actually wait out the timeout rather
	// than short-circuit. context.WithTimeout never fires before its deadline, so
	// elapsed >= timeout holds regardless of host load — and it catches a real
	// regression (dropped timeout plumbing, or an early exit) that the old
	// ceiling-only assertion missed.
	if elapsed < timeout {
		t.Errorf("watch returned after %v, before the %v timeout — timeout not respected", elapsed, timeout)
	}
	// Generous sanity ceiling: flags a wildly-wrong timeout (e.g. seconds, not
	// millis) while staying immune to -race scheduler jitter. A true
	// timeout-ignore regression hangs and is caught by `go test`'s own deadline,
	// not here, so this bound is deliberately loose.
	if elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v (want < 5s)", elapsed)
	}
	// The loop polled at least once before timing out: the queued state rendered.
	if !strings.Contains(stdout.String(), "STATE\tqueued") {
		t.Errorf("expected queued state to render before timeout; got %s", stdout.String())
	}
}

// TestIsTerminalState — direct check of the state classifier.
func TestIsTerminalState(t *testing.T) {
	terminal := []string{"delivered", "failed"}
	nonTerminal := []string{"queued", "delivering"}
	for _, s := range terminal {
		if !isTerminalState(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range nonTerminal {
		if isTerminalState(s) {
			t.Errorf("%q should NOT be terminal", s)
		}
	}
}
