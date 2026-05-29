package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
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
	}
}

// withSuccessfulDelivery installs a fake tmuxRunner that captures the body
// passed via load-buffer and replays it on capture-pane, so the verify
// token (the message's "id <public_id>") is found on the first attempt.
func withSuccessfulDelivery(t *testing.T) {
	t.Helper()
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

func TestServe_RefusesWhenAgentUnregistered(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stderr bytes.Buffer
	exit := runServeWithStore(context.Background(), s, fastOpts("ghost"),
		log.New(&stderr, "", 0), io.Discard, &stderr)
	if exit != exitUnavailable {
		t.Errorf("exit = %d, want %d", exit, exitUnavailable)
	}
	if !strings.Contains(stderr.String(), "not registered") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestServe_RefusesWhenPaneEmpty(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "")

	var stderr bytes.Buffer
	exit := runServeWithStore(ctx, s, fastOpts("bob"),
		log.New(&stderr, "", 0), io.Discard, &stderr)
	if exit != exitUnavailable {
		t.Errorf("exit = %d, want %d", exit, exitUnavailable)
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
