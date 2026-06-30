package cli

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"bytes"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
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
// /compact control message is delivered, the next queued row is held by the
// stability-gate (#622) until the pane settles, then delivered in order. The
// gate is adaptive (wait-for-stably-idle, PostCompactPause as a ceiling), not a
// fixed minimum — the "land follow-up after compaction settles" property.
//
// The gap is measured via the store's `delivered_at` column (stamped
// inside MarkDelivered at the actual transition moment) rather than
// `time.Now()` at the test's polling-observation time (#127). The
// polling-time path was flaky under load because the test loop polls
// at 2ms cadence; the observed gap could lag the actual mailman gap
// by up to ~4ms (double-sided jitter — late observation of compactAt
// + early observation of resumeAt). Using `delivered_at` makes the
// measurement reflect what the mailman actually did, not what the
// poller managed to observe.
func TestServe_PostCompactPauseDelaysNextDelivery(t *testing.T) {
	withSuccessfulDelivery(t)
	fastStabilityGate(t)

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	compactRes, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "/compact", Kind: store.KindControl,
	})
	if err != nil {
		t.Fatalf("insert compact: %v", err)
	}
	resumeRes, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "resume work on the bus", Kind: store.KindMessage,
	})
	if err != nil {
		t.Fatalf("insert resume: %v", err)
	}

	opts := fastOpts("bob")
	opts.PostCompactPause = 30 * time.Millisecond // stability-gate ceiling for the test

	stopCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	logbuf := &bytes.Buffer{}
	logger := log.New(logbuf, "[mailman/test] ", 0)
	done := make(chan int, 1)
	go func() { done <- runServeWithStore(stopCtx, s, opts, logger, io.Discard, io.Discard) }()

	// Poll until both messages reach `delivered`. The polling decides
	// when to stop waiting; the gap measurement comes from the store's
	// own `delivered_at` timestamps (recorded inside MarkDelivered),
	// not from the test's polling observation time.
	deadline := time.Now().Add(2*time.Second + opts.PostCompactPause)
	bothDelivered := false
	for time.Now().Before(deadline) {
		msgs, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(msgs) >= 2 {
			bothDelivered = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	<-done

	if !bothDelivered {
		t.Fatalf("not both delivered within deadline; log=%s", logbuf.String())
	}

	// Re-fetch both messages by ID to get their final delivered_at
	// timestamps. GetMessage emits the raw ISO string from the DB.
	compact, err := s.GetMessage(ctx, compactRes.PublicID)
	if err != nil {
		t.Fatalf("get compact: %v", err)
	}
	resume, err := s.GetMessage(ctx, resumeRes.PublicID)
	if err != nil {
		t.Fatalf("get resume: %v", err)
	}
	if !compact.DeliveredAt.Valid || !resume.DeliveredAt.Valid {
		t.Fatalf("delivered_at empty: compact=%v resume=%v",
			compact.DeliveredAt, resume.DeliveredAt)
	}
	compactAt, err := time.Parse("2006-01-02T15:04:05.000Z", compact.DeliveredAt.String)
	if err != nil {
		t.Fatalf("parse compact.delivered_at %q: %v", compact.DeliveredAt.String, err)
	}
	resumeAt, err := time.Parse("2006-01-02T15:04:05.000Z", resume.DeliveredAt.String)
	if err != nil {
		t.Fatalf("parse resume.delivered_at %q: %v", resume.DeliveredAt.String, err)
	}
	// #622: the post-/compact wait is now an adaptive stability-gate, not a fixed
	// minimum — the resume is held until the pane reads stably idle (up to
	// PostCompactPause as a ceiling). So we assert ORDERING (resume after the
	// compact) + that the stability-gate engaged, not a fixed gap.
	if resumeAt.Before(compactAt) {
		t.Errorf("resume delivered before compact (resumeAt=%s compactAt=%s); ordering must hold",
			resumeAt, compactAt)
	}
	if !strings.Contains(logbuf.String(), "post_compact_stability_wait") {
		t.Errorf("expected post_compact_stability_wait log line; got %s", logbuf.String())
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
			if strings.Contains(logbuf.String(), "post_compact_stability_wait") {
				t.Errorf("post-compact stability-gate should NOT fire on /help; log=%s",
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

// fastStabilityGate shrinks the #622 post-/compact stability-gate's poll cadence
// + AgentState temporal delta so tests don't wait the production 1s/poll.
func fastStabilityGate(t *testing.T) {
	t.Helper()
	op, od := postCompactPollEvery, postCompactStableDebounce
	postCompactPollEvery = time.Millisecond
	postCompactStableDebounce = 2
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() {
		postCompactPollEvery = op
		postCompactStableDebounce = od
		tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta)
	})
}

// TestWaitForStableIdle pins the #622 stability-gate primitive: it settles only
// once the pane reads StateIdle for `debounce` consecutive polls, returns
// settled=false at the maxWait ceiling when the pane never settles (e.g. a
// /compact still in progress), and returns stopped on ctx-cancel.
func TestWaitForStableIdle(t *testing.T) {
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	// An idle pane: cursor (2/2) at the prompt sentinel on row 2, content stable.
	idleRunner := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "capture-pane":
			return []byte("some history\n" + tmuxio.PromptSentinel + "\nfooter\n"), nil
		case "display-message":
			return []byte("2/1"), nil // cursor at sentinel col (2) on the prompt row (1)
		}
		return nil, nil
	}
	// A pane mid-/compact: AgentState reads StateAtRestInCompaction (never idle).
	compactingRunner := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("✻ Compacting conversation… (7s · ↑ 2.9k tokens)\n  ▰▰▰▱▱\n"), nil
		}
		return nil, nil
	}

	t.Run("settles when pane stably idle", func(t *testing.T) {
		prev := tmuxio.SetTmuxRunner(idleRunner)
		t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
		settled, stopped := waitForStableIdle(context.Background(), "%3", time.Second, time.Millisecond, 0, 2)
		if !settled || stopped {
			t.Errorf("settled=%v stopped=%v, want settled=true stopped=false", settled, stopped)
		}
	})

	t.Run("ceiling when pane never idle (mid-compact)", func(t *testing.T) {
		prev := tmuxio.SetTmuxRunner(compactingRunner)
		t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
		settled, stopped := waitForStableIdle(context.Background(), "%3", 5*time.Millisecond, time.Millisecond, 0, 2)
		if settled || stopped {
			t.Errorf("settled=%v stopped=%v, want settled=false (ceiling) stopped=false", settled, stopped)
		}
	})

	t.Run("stopped on ctx cancel", func(t *testing.T) {
		prev := tmuxio.SetTmuxRunner(compactingRunner)
		t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, stopped := waitForStableIdle(ctx, "%3", time.Second, time.Millisecond, 0, 2)
		if !stopped {
			t.Errorf("stopped=%v, want true (ctx cancelled)", stopped)
		}
	})
}
