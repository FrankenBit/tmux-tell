package main

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"bytes"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

// TestServe_DeliversControlMessageViaSendKeysOnly asserts that a queued
// store.KindControl row is delivered by typing the body directly through
// `send-keys -l` followed by Enter, without ever touching load-buffer or
// capture-pane.
func TestServe_DeliversControlMessageViaSendKeysOnly(t *testing.T) {
	var (
		mu        sync.Mutex
		calls     []string
		litBody   string
		bufferUse bool
		captured  bool
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "send-keys":
			// Look for the literal-typing call: `send-keys -t %3 -l "/compact"`
			for i, a := range args {
				if a == "-l" && i+1 < len(args) {
					litBody = args[i+1]
				}
			}
		case "load-buffer":
			bufferUse = true
		case "capture-pane":
			captured = true
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "/compact", Kind: store.KindControl,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stop, wait, _ := runServeInBackground(t, s, fastOpts("bob"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(all) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	mu.Lock()
	defer mu.Unlock()

	delivered, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "bob", State: store.StateDelivered, Limit: 10,
	})
	if len(delivered) != 1 {
		t.Fatalf("delivered = %d, want 1; calls=%v", len(delivered), calls)
	}
	if bufferUse {
		t.Errorf("load-buffer should NOT be used for control kind; calls=%v", calls)
	}
	if captured {
		t.Errorf("capture-pane (verify) should NOT run for control kind; calls=%v", calls)
	}
	if litBody != "/compact" {
		t.Errorf("send-keys -l body = %q, want %q; calls=%v", litBody, "/compact", calls)
	}
	// Sanity: at least one send-keys Enter must have followed.
	var sawEnter bool
	for _, c := range calls {
		if strings.HasPrefix(c, "send-keys") && strings.HasSuffix(c, "Enter") {
			sawEnter = true
		}
	}
	if !sawEnter {
		t.Errorf("no send-keys Enter observed; calls=%v", calls)
	}
}

// TestServe_PostCompactPauseDelaysNextDelivery asserts that when a
// /compact control message is delivered, the next queued row is held
// for at least PostCompactPause before its delivery starts. This is
// the "land follow-up after compaction settles" property.
func TestServe_PostCompactPauseDelaysNextDelivery(t *testing.T) {
	withSuccessfulDelivery(t)

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "/compact", Kind: store.KindControl,
	}); err != nil {
		t.Fatalf("insert compact: %v", err)
	}
	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "resume work on the bus", Kind: store.KindMessage,
	}); err != nil {
		t.Fatalf("insert resume: %v", err)
	}

	opts := fastOpts("bob")
	opts.PostCompactPause = 80 * time.Millisecond

	stopCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	logbuf := &bytes.Buffer{}
	logger := log.New(logbuf, "[mailman/test] ", 0)
	done := make(chan int, 1)
	go func() { done <- runServeWithStore(stopCtx, s, opts, logger, io.Discard, io.Discard) }()

	// Wait until /compact is marked delivered.
	deadline := time.Now().Add(time.Second)
	var compactAt time.Time
	for time.Now().Before(deadline) {
		msgs, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(msgs) >= 1 {
			compactAt = time.Now()
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if compactAt.IsZero() {
		t.Fatalf("/compact never delivered; log=%s", logbuf.String())
	}

	// Now wait for the resume to be delivered and capture how long it took.
	var resumeAt time.Time
	for time.Now().Before(deadline.Add(opts.PostCompactPause + time.Second)) {
		msgs, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(msgs) >= 2 {
			resumeAt = time.Now()
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	<-done

	if resumeAt.IsZero() {
		t.Fatalf("resume never delivered; log=%s", logbuf.String())
	}
	gap := resumeAt.Sub(compactAt)
	if gap < opts.PostCompactPause {
		t.Errorf("gap = %s, want >= %s (post-compact pause)", gap, opts.PostCompactPause)
	}
	if !strings.Contains(logbuf.String(), "post_compact_pause") {
		t.Errorf("expected post_compact_pause log line; got %s", logbuf.String())
	}
}

// TestServe_NonCompactControlDoesNotPause asserts that the pause is
// strictly tied to /compact — other control commands (e.g. /help) do
// NOT trigger the long delay.
func TestServe_NonCompactControlDoesNotPause(t *testing.T) {
	withSuccessfulDelivery(t)

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	// /help is a control row but NOT compact.
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "/help", Kind: store.KindControl,
	})
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "follow-up", Kind: store.KindMessage,
	})

	opts := fastOpts("bob")
	// Set the pause LONG. If the implementation incorrectly applied it to
	// /help, the resume would not deliver within the deadline below.
	opts.PostCompactPause = 5 * time.Second

	stop, wait, logbuf := runServeInBackground(t, s, opts)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		msgs, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(msgs) == 2 {
			stop()
			wait()
			if strings.Contains(logbuf.String(), "post_compact_pause") {
				t.Errorf("post-compact pause should NOT fire on /help; log=%s",
					logbuf.String())
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()
	t.Fatalf("both messages should have been delivered quickly; log=%s",
		logbuf.String())
}
