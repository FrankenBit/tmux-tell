package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/metrics"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// capOpts builds cap-enabled serve opts from the fast test defaults: the cap is
// ON (ProviderCapDisabled=false), with a generous TTL so the seeded workers stay
// fresh for the test, a 1ms recheck so a deferral loop turns over quickly, and a
// 1h observe interval so the recipient's own self-probe fires at most once.
func capOpts(agent string, cap int) serveOpts {
	o := fastOpts(agent)
	o.ProviderCapDisabled = false
	o.MaxConcurrentPerProvider = cap
	o.ProviderCapTTL = 30 * time.Second
	o.ProviderCapRecheckInterval = time.Millisecond
	o.ObservedStateInterval = time.Hour
	return o
}

// seedWorkers registers agents on a provider and stamps them fresh-working, so
// the cross-mailman cap counts them. They have no running mailman in the test —
// their observed_state stays put for the test's duration.
func seedWorkers(t *testing.T, s *store.Store, provider string, names ...string) {
	t.Helper()
	ctx := context.Background()
	for _, n := range names {
		if err := s.UpsertAgent(ctx, n, "%1"); err != nil {
			t.Fatalf("upsert %s: %v", n, err)
		}
		if err := s.SetProvider(ctx, n, provider); err != nil {
			t.Fatalf("set provider %s: %v", n, err)
		}
		if err := s.SetObservedState(ctx, n, "working", time.Now()); err != nil {
			t.Fatalf("set state %s: %v", n, err)
		}
	}
}

// TestServe_ProviderCap_DefersAtCap is the #448 load-bearing AC: when the
// per-provider working-count is already at the cap, the mailman defers delivery
// (logs provider_cap_deferred, does not paste). active.Provider defaults to
// "anthropic" in-package, so the 3 seeded anthropic workers fill the cap of 3.
func TestServe_ProviderCap_DefersAtCap(t *testing.T) {
	withSuccessfulDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil { // recipient with a pane
		t.Fatalf("upsert bob: %v", err)
	}
	seedWorkers(t, s, "anthropic", "w1", "w2", "w3") // 3 working == cap
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stop, wait, logbuf := runServeInBackground(t, s, capOpts("bob", 3))
	waitFor(t, time.Second,
		func() bool { return strings.Contains(logbuf.String(), "provider_cap_deferred") },
		"expected a provider_cap_deferred log within 1s")
	delivered, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
	stop()
	wait()
	if len(delivered) != 0 {
		t.Errorf("delivered=%d while at the provider cap, want 0 (the message must stay queued)", len(delivered))
	}
}

// TestServe_ProviderCap_DeliversUnderCap pins the open-slot half: with the
// working-count below the cap, the gate passes and delivery proceeds normally.
func TestServe_ProviderCap_DeliversUnderCap(t *testing.T) {
	withSuccessfulDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	seedWorkers(t, s, "anthropic", "w1", "w2") // 2 working < cap 3
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stop, wait, _ := runServeInBackground(t, s, capOpts("bob", 3))
	waitFor(t, 2*time.Second, func() bool {
		d, _ := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
		return len(d) == 1
	}, "expected delivery under the provider cap within 2s")
	stop()
	wait()
}

func TestServe_ProviderCap_MetricsInflightGaugeInitializesZero(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}

	m := metrics.New()
	opts := capOpts("bob", 1)
	opts.Metrics = m

	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	waitFor(t, time.Second, func() bool {
		families, err := m.Registry().Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, fam := range families {
			if fam.GetName() != "tmux_tell_provider_defer_inflight" {
				continue
			}
			for _, metric := range fam.GetMetric() {
				if labelsMatch(metric.GetLabel(), map[string]string{"provider": "anthropic"}) {
					return metric.GetGauge().GetValue() == 0
				}
			}
		}
		return false
	}, "expected provider-cap inflight gauge series to initialize at 0")
}

func TestServe_ProviderCap_MetricsInflightGaugeTracksDeferredMessages(t *testing.T) {
	withSuccessfulDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	seedWorkers(t, s, "anthropic", "w1") // 1 working == cap
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	m := metrics.New()
	opts := capOpts("bob", 1)
	opts.Metrics = m

	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	waitFor(t, time.Second, func() bool {
		return gatherCounter(t, m, "tmux_tell_provider_defer_total",
			map[string]string{"provider": "anthropic"}) >= 2
	}, "expected repeat provider-cap deferrals within 1s")
	if got := gatherGauge(t, m, "tmux_tell_provider_defer_inflight",
		map[string]string{"provider": "anthropic"}); got != 1 {
		t.Fatalf("inflight gauge while repeatedly deferred = %v, want 1", got)
	}

	if err := s.SetObservedState(ctx, "w1", "idle", time.Now()); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	waitDelivered(t, s, "bob")

	if got := gatherGauge(t, m, "tmux_tell_provider_defer_inflight",
		map[string]string{"provider": "anthropic"}); got != 0 {
		t.Errorf("inflight gauge after cap pass = %v, want 0", got)
	}
	if got := gatherHistCount(t, m, "tmux_tell_provider_defer_wait_seconds",
		map[string]string{"provider": "anthropic"}); got != 1 {
		t.Errorf("provider defer-wait samples = %d, want 1", got)
	}
}

func TestServe_ProviderCap_MetricsInflightGaugePrunesExternalQueueRemoval(t *testing.T) {
	withSuccessfulDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	seedWorkers(t, s, "anthropic", "w1") // 1 working == cap
	res, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	m := metrics.New()
	opts := capOpts("bob", 1)
	opts.Metrics = m

	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	waitFor(t, time.Second, func() bool {
		return gatherGauge(t, m, "tmux_tell_provider_defer_inflight",
			map[string]string{"provider": "anthropic"}) == 1
	}, "expected provider-cap inflight gauge to reach 1")

	if _, err := s.DB().ExecContext(ctx, `UPDATE messages SET state = ? WHERE public_id = ?`,
		store.StateAcknowledged, res.PublicID); err != nil {
		t.Fatalf("externally acknowledge deferred row: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		return gatherGauge(t, m, "tmux_tell_provider_defer_inflight",
			map[string]string{"provider": "anthropic"}) == 0
	}, "expected provider-cap inflight gauge to prune externally acknowledged row")
}
