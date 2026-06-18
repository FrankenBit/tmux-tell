package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/metrics"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
	dto "github.com/prometheus/client_model/go"
)

// gatherGauge returns the current value of a gauge series identified by name +
// an exact label-value set, or 0 when the series is absent.
func gatherGauge(t *testing.T, m *metrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, metric := range fam.GetMetric() {
			if labelsMatch(metric.GetLabel(), labels) {
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// gatherCounter returns the value of a counter series identified by name +
// an exact label-value set, or 0 when the series is absent (the
// never-incremented case reads as zero, like Prometheus itself).
func gatherCounter(t *testing.T, m *metrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, metric := range fam.GetMetric() {
			if labelsMatch(metric.GetLabel(), labels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// gatherHistCount returns the observation count of a histogram series, or 0
// when absent.
func gatherHistCount(t *testing.T, m *metrics.Metrics, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, metric := range fam.GetMetric() {
			if labelsMatch(metric.GetLabel(), labels) {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// labelsMatch reports whether the metric's label set equals the wanted set.
// The want maps are built complete for each metric, so this is an exact match.
func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, lp := range got {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}

func waitDelivered(t *testing.T, s *store.Store, agent string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(context.Background(), store.ListFilter{ToAgent: agent, State: store.StateDelivered, Limit: 10})
		if len(d) == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("message never reached delivered for %s", agent)
}

// TestServe_Metrics_RecordsDeliveredLatencyVerifyLoop pins the happy-path
// wiring: a verified delivery increments messages_total{state=delivered},
// observes one delivery-latency sample + one verify-attempt sample for the
// recipient, and the loop counter advances.
func TestServe_Metrics_RecordsDeliveredLatencyVerifyLoop(t *testing.T) {
	withSuccessfulDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"})

	m := metrics.New()
	opts := fastOpts("bob")
	opts.Metrics = m

	stop, wait, _ := runServeInBackground(t, s, opts)
	waitDelivered(t, s, "bob")
	stop()
	wait()

	if got := gatherCounter(t, m, "tmux_tell_messages_total",
		map[string]string{"from": "alice", "to": "bob", "state": "delivered"}); got != 1 {
		t.Errorf("messages_total delivered = %v, want 1", got)
	}
	if got := gatherHistCount(t, m, "tmux_tell_delivery_latency_seconds",
		map[string]string{"recipient": "bob"}); got != 1 {
		t.Errorf("delivery_latency sample count = %d, want 1", got)
	}
	if got := gatherHistCount(t, m, "tmux_tell_delivery_verify_attempt_seconds",
		map[string]string{"recipient": "bob"}); got != 1 {
		t.Errorf("verify_attempt sample count = %d, want 1", got)
	}
	if got := gatherCounter(t, m, "tmux_tell_mailman_loop_iterations_total",
		map[string]string{"agent": "bob"}); got < 1 {
		t.Errorf("loop iterations = %v, want >= 1", got)
	}
}

// TestServe_Metrics_RecordsUnverified pins the soft-fail branch: the message
// reaches `delivered` with verified=0, so messages_total carries the
// delivered_in_input_box state and the verify-attempt loop is still observed
// (it ran to budget exhaustion).
func TestServe_Metrics_RecordsUnverified(t *testing.T) {
	withUnverifiedDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"})

	m := metrics.New()
	opts := fastOpts("bob")
	opts.Metrics = m

	stop, wait, _ := runServeInBackground(t, s, opts)
	waitDelivered(t, s, "bob")
	stop()
	wait()

	if got := gatherCounter(t, m, "tmux_tell_messages_total",
		map[string]string{"from": "alice", "to": "bob", "state": "delivered_in_input_box"}); got != 1 {
		t.Errorf("messages_total delivered_in_input_box = %v, want 1", got)
	}
	if got := gatherHistCount(t, m, "tmux_tell_delivery_verify_attempt_seconds",
		map[string]string{"recipient": "bob"}); got < 1 {
		t.Errorf("verify_attempt sample count = %d, want >= 1", got)
	}
}

// TestServe_Metrics_PasteUnsafeAbort pins the abort boundary: a paste-unsafe
// pane state increments paste_unsafe_aborts_total{reason=awaiting_operator}
// and — because the message reverts to queued rather than reaching a terminal
// outcome — leaves messages_total untouched.
func TestServe_Metrics_PasteUnsafeAbort(t *testing.T) {
	popupPane := "history\n" + tmuxio.PromptSentinel + "operator typing\n" +
		"footer with " + tmuxio.AwaitingOperatorMarker + "\n"
	var mu sync.Mutex
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "capture-pane":
			return []byte(popupPane), nil
		case "display-message":
			return []byte("20/1\n"), nil // cursor past sentinel → AwaitingOperator
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "x"})

	m := metrics.New()
	opts := fastOpts("bob")
	opts.PrePasteSafetyDisabled = false // enable the check (override fastOpts default)
	opts.Metrics = m

	stop, wait, _ := runServeInBackground(t, s, opts)
	time.Sleep(50 * time.Millisecond)
	stop()
	wait()

	if got := gatherCounter(t, m, "tmux_tell_paste_unsafe_aborts_total",
		map[string]string{"agent": "bob", "reason": "awaiting_operator"}); got < 1 {
		t.Errorf("paste_unsafe_aborts awaiting_operator = %v, want >= 1", got)
	}
	// No terminal outcome → no messages_total of any state.
	for _, st := range []string{"delivered", "delivered_in_input_box", "failed"} {
		if got := gatherCounter(t, m, "tmux_tell_messages_total",
			map[string]string{"from": "alice", "to": "bob", "state": st}); got != 0 {
			t.Errorf("messages_total state=%s = %v, want 0 (abort is not a terminal outcome)", st, got)
		}
	}
}

// freeTCPAddr returns a currently-free loopback address. The small bind→close
// race is acceptable for a test: the enabled phase re-binds it immediately.
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func getMetrics(addr string) (int, string, error) {
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/metrics")
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

// TestServe_Metrics_EndpointGatedByAddr is the end-to-end contrast for the
// "configurable port; absent → disabled" AC. Phase A (MetricsAddr set) stands
// up a live /metrics serving valid exposition with the delivered series;
// after shutdown the port goes quiet. Phase B (MetricsAddr empty) reuses the
// same address and proves nothing is listening — the flag is what gates the
// endpoint, with no behavior change for deploys that leave it unset.
func TestServe_Metrics_EndpointGatedByAddr(t *testing.T) {
	addr := freeTCPAddr(t)

	// --- Phase A: enabled ---
	withSuccessfulDelivery(t)
	sA, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = sA.Close() })
	ctx := context.Background()
	_ = sA.UpsertAgent(ctx, "alice", "%1")
	_ = sA.UpsertAgent(ctx, "bob", "%3")
	_, _ = sA.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"})

	optsA := fastOpts("bob")
	optsA.MetricsAddr = addr // production path: runServeWithStore builds + serves
	stopA, waitA, _ := runServeInBackground(t, sA, optsA)
	waitDelivered(t, sA, "bob")

	var code int
	var body string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, b, err := getMetrics(addr)
		if err == nil && c == http.StatusOK && strings.Contains(b, "tmux_tell_messages_total") {
			code, body = c, b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code != http.StatusOK {
		t.Fatalf("enabled /metrics never served 200 with exposition (last code=%d)", code)
	}
	if !strings.Contains(body, `state="delivered"`) {
		t.Errorf("exposition missing delivered series; got:\n%s", body)
	}
	stopA()
	waitA()
	// Wait for the listener to fully release before reusing the port.
	releaseBy := time.Now().Add(2 * time.Second)
	for time.Now().Before(releaseBy) {
		if _, _, err := getMetrics(addr); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// --- Phase B: disabled (same addr, no flag) ---
	sB, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = sB.Close() })
	_ = sB.UpsertAgent(ctx, "alice", "%1")
	_ = sB.UpsertAgent(ctx, "bob", "%3")
	_, _ = sB.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"})

	optsB := fastOpts("bob") // MetricsAddr == "" → endpoint disabled
	stopB, waitB, _ := runServeInBackground(t, sB, optsB)
	waitDelivered(t, sB, "bob") // delivery still works with metrics disabled
	if _, _, err := getMetrics(addr); err == nil {
		t.Errorf("disabled mailman should not serve /metrics on %s, but the request succeeded", addr)
	}
	stopB()
	waitB()
}

// TestServe_Metrics_MailmanStuck_ParkAndUnpark pins the #300 gauge wiring:
// the serve loop sets tmux_tell_mailman_stuck=1 when the agent parks and
// =0 when the stuck state is cleared externally (the register --force path).
// Uses StuckThreshold=1 and a phased runner to avoid immediate re-parking
// after the clear: Phase 1 returns "can't find pane" (parks immediately);
// Phase 2 switches to a non-pane-not-found error (resets consecutivePaneFails
// on each attempt, preventing re-park) so the gauge stays at 0 after clear.
func TestServe_Metrics_MailmanStuck_ParkAndUnpark(t *testing.T) {
	var mu sync.Mutex
	parkMode := true // Phase 1 = can't-find-pane; Phase 2 = other error (no re-park)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if len(args) == 0 || args[0] != "capture-pane" {
			return nil, nil
		}
		mu.Lock()
		inParkMode := parkMode
		mu.Unlock()
		if inParkMode {
			return nil, &errString{"exit status 1: can't find pane: %3"}
		}
		// Non-can't-find-pane error resets consecutivePaneFails (lines 957-961)
		// so the agent never re-parks after the stuck state is cleared.
		return nil, &errString{"exit status 1: tmux: socket gone (not pane-not-found)"}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"})

	m := metrics.New()
	opts := fastOpts("bob")
	opts.PrePasteSafetyDisabled = false
	opts.StuckThreshold = 1
	opts.StuckPollInterval = 5 * time.Millisecond
	opts.Metrics = m

	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	// Phase 1: wait for park; gauge must reach 1.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if gatherGauge(t, m, "tmux_tell_mailman_stuck",
			map[string]string{"agent": "bob", "reason": store.StuckReasonPaneNotFound}) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := gatherGauge(t, m, "tmux_tell_mailman_stuck",
		map[string]string{"agent": "bob", "reason": store.StuckReasonPaneNotFound}); got != 1 {
		t.Errorf("stuck gauge after park = %v, want 1", got)
	}

	// Phase 2: switch to non-park-mode runner, then clear stuck. The next
	// loop iteration detects the cleared stuck_reason and drops the gauge to 0.
	// The non-park-mode runner prevents immediate re-park.
	mu.Lock()
	parkMode = false
	mu.Unlock()
	if err := s.ClearStuck(ctx, "bob"); err != nil {
		t.Fatalf("clear stuck: %v", err)
	}
	clearDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(clearDeadline) {
		if gatherGauge(t, m, "tmux_tell_mailman_stuck",
			map[string]string{"agent": "bob", "reason": store.StuckReasonPaneNotFound}) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := gatherGauge(t, m, "tmux_tell_mailman_stuck",
		map[string]string{"agent": "bob", "reason": store.StuckReasonPaneNotFound}); got != 0 {
		t.Errorf("stuck gauge after unpark = %v, want 0", got)
	}
}

// TestServe_Metrics_MailmanStuck_StartupWithParkedAgent pins the startup-with-
// already-parked scenario: a mailman started against an agent whose stuck_reason
// is already set (previous run parked it) must immediately reflect the parked
// state in the gauge without waiting for another park transition.
func TestServe_Metrics_MailmanStuck_StartupWithParkedAgent(t *testing.T) {
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		return nil, &errString{"exit status 1: can't find pane: %3"}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if err := s.SetStuck(ctx, "bob", store.StuckReasonPaneNotFound); err != nil {
		t.Fatalf("seed stuck: %v", err)
	}

	m := metrics.New()
	opts := fastOpts("bob")
	opts.StuckPollInterval = 5 * time.Millisecond
	opts.Metrics = m

	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	// Gauge must reach 1 within a few loop iterations (no delivery needed).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gatherGauge(t, m, "tmux_tell_mailman_stuck",
			map[string]string{"agent": "bob", "reason": store.StuckReasonPaneNotFound}) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := gatherGauge(t, m, "tmux_tell_mailman_stuck",
		map[string]string{"agent": "bob", "reason": store.StuckReasonPaneNotFound}); got != 1 {
		t.Errorf("stuck gauge at startup with parked agent = %v, want 1", got)
	}
}

// TestPasteUnsafeReason pins the reason-label mapping that feeds the
// paste_unsafe_aborts metric — the closed label set the Grafana panel
// enumerates.
func TestPasteUnsafeReason(t *testing.T) {
	cases := []struct {
		state tmuxio.State
		err   error
		want  string
	}{
		{tmuxio.StateAwaitingOperator, nil, "awaiting_operator"},
		{tmuxio.StateAtRestInCompaction, nil, "compaction"},
		{tmuxio.StateRateLimited, nil, "rate_limited"},
		{tmuxio.StateUnknown, nil, "unknown"},
		{tmuxio.StateIdle, nil, "unknown"}, // non-unsafe state never reaches here; maps to unknown
		{tmuxio.StateIdle, context.DeadlineExceeded, "probe_failed"},
		{tmuxio.StateUnknown, context.DeadlineExceeded, "probe_failed"}, // probe error outranks state
	}
	for _, c := range cases {
		if got := pasteUnsafeReason(c.state, c.err); got != c.want {
			t.Errorf("pasteUnsafeReason(%v, %v) = %q, want %q", c.state, c.err, got, c.want)
		}
	}
}
