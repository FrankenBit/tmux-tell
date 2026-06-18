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

func TestServe_UsageLimitedDefersAndReportsMetrics(t *testing.T) {
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
		PromptSentinel:    tmuxio.CodexPromptSentinel,
		UsageLimitPattern: `■ You've hit your usage limit`,
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
			return []byte("■ You've hit your usage limit. Visit settings to purchase more credits."), nil
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

	labels := map[string]string{"agent": "bob", "provider": "anthropic"}
	waitFor(t, 2*time.Second, func() bool {
		return gatherGauge(t, m, "tmux_tell_chamber_usage_limited_seconds", labels) > 0
	}, "expected usage-limited age gauge to become positive")

	if got := gatherGauge(t, m, "tmux_tell_chamber_usage_limited_seconds", labels); got <= 0 {
		t.Fatalf("usage-limited age gauge = %v, want > 0", got)
	}

	delivered, err := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
	if err != nil {
		t.Fatalf("list delivered: %v", err)
	}
	if len(delivered) != 0 {
		t.Fatalf("delivered = %d while usage-limited, want 0", len(delivered))
	}
}
