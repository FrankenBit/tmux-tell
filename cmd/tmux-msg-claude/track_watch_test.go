package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
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
func TestRunTrackWatch_TimeoutExits(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = ctx

	var stdout, stderr bytes.Buffer
	start := time.Now()
	exit := runTrackWatch(s, res.PublicID, "text",
		5*time.Millisecond, 30*time.Millisecond, &stdout, &stderr)
	elapsed := time.Since(start)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("timeout took too long: %v", elapsed)
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
