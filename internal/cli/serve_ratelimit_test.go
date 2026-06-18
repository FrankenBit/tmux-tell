package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/metrics"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

func TestServe_RateLimitedDefersAndReportsMetrics(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	prevProfile := tmuxio.ActivePaneProfile()
	tmuxio.SetActivePaneProfile(tmuxio.PaneProfile{
		PromptSentinel:   tmuxio.CodexPromptSentinel,
		RateLimitPattern: `Rate limited.*?retry after (?P<retry_seconds>\d+ms)`,
	})
	t.Cleanup(func() { tmuxio.SetActivePaneProfile(prevProfile) })

	prevRunner := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			if strings.Contains(strings.Join(args, " "), "#{pane_in_mode}") {
				return []byte("0"), nil
			}
			return []byte("0/0"), nil
		case "capture-pane":
			return []byte("Rate limited retry after 400ms"), nil
		default:
			return nil, nil
		}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prevRunner) })

	m := metrics.New()
	opts := fastOpts("bob")
	opts.GateDisabled = false
	opts.Metrics = m
	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	waitFor(t, 2*time.Second, func() bool {
		return gatherGauge(t, m, "tmux_tell_chamber_rate_limited_seconds",
			map[string]string{"agent": "bob", "provider": "anthropic"}) > 0
	}, "expected rate-limited age gauge to become positive")

	labels := map[string]string{"agent": "bob", "provider": "anthropic"}
	firstAge := gatherGauge(t, m, "tmux_tell_chamber_rate_limited_seconds", labels)
	firstRetry := gatherGauge(t, m, "tmux_tell_chamber_rate_limit_retry_after_seconds", labels)
	if firstAge <= 0 {
		t.Fatalf("initial rate-limited age = %v, want > 0", firstAge)
	}
	if firstRetry <= 0 {
		t.Fatalf("initial retry-after = %v, want > 0", firstRetry)
	}

	time.Sleep(200 * time.Millisecond)

	secondAge := gatherGauge(t, m, "tmux_tell_chamber_rate_limited_seconds", labels)
	secondRetry := gatherGauge(t, m, "tmux_tell_chamber_rate_limit_retry_after_seconds", labels)
	if secondAge <= firstAge {
		t.Fatalf("rate-limited age did not advance: first=%v second=%v", firstAge, secondAge)
	}
	if secondRetry >= firstRetry {
		t.Fatalf("retry-after did not count down: first=%v second=%v", firstRetry, secondRetry)
	}

	delivered, err := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
	if err != nil {
		t.Fatalf("list delivered: %v", err)
	}
	if len(delivered) != 0 {
		t.Fatalf("delivered = %d while rate-limited, want 0", len(delivered))
	}
}
