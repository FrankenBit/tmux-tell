package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

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
