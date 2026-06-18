package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// histSampleCount gathers the registry and returns the observation count for
// the named histogram restricted to a single label value (the only label on
// the delivery/verify histograms). Fails the test if the series is absent.
func histSampleCount(t *testing.T, m *Metrics, name, labelValue string) uint64 {
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
			for _, lp := range metric.GetLabel() {
				if lp.GetValue() == labelValue {
					return metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	t.Fatalf("histogram %q with label value %q not found", name, labelValue)
	return 0
}

func TestRecordDelivery_IncrementsByLabelSet(t *testing.T) {
	m := New()
	m.RecordDelivery("alice", "bob", StateDelivered)
	m.RecordDelivery("alice", "bob", StateDelivered)
	m.RecordDelivery("alice", "bob", StateFailed)
	m.RecordDelivery("carol", "bob", StateDeliveredInInputBox)

	if got := testutil.ToFloat64(m.messagesTotal.WithLabelValues("alice", "bob", StateDelivered)); got != 2 {
		t.Errorf("alice→bob delivered = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.messagesTotal.WithLabelValues("alice", "bob", StateFailed)); got != 1 {
		t.Errorf("alice→bob failed = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.messagesTotal.WithLabelValues("carol", "bob", StateDeliveredInInputBox)); got != 1 {
		t.Errorf("carol→bob delivered_in_input_box = %v, want 1", got)
	}
	// A label set never recorded must read as zero, not error.
	if got := testutil.ToFloat64(m.messagesTotal.WithLabelValues("alice", "carol", StateDelivered)); got != 0 {
		t.Errorf("untouched series = %v, want 0", got)
	}
}

func TestObserveHistograms_CountObservations(t *testing.T) {
	m := New()
	m.ObserveDeliveryLatency("bob", 0.3)
	m.ObserveDeliveryLatency("bob", 12)
	m.ObserveVerifyAttempt("bob", 1.5)

	if c := histSampleCount(t, m, "tmux_tell_delivery_latency_seconds", "bob"); c != 2 {
		t.Errorf("delivery latency sample count = %d, want 2", c)
	}
	if c := histSampleCount(t, m, "tmux_tell_delivery_verify_attempt_seconds", "bob"); c != 1 {
		t.Errorf("verify attempt sample count = %d, want 1", c)
	}

	// #507: provider-cap defer-wait histogram, labeled by provider.
	m.ObserveProviderDeferWait("anthropic", 2.5)
	m.ObserveProviderDeferWait("anthropic", 40)
	if c := histSampleCount(t, m, "tmux_tell_provider_defer_wait_seconds", "anthropic"); c != 2 {
		t.Errorf("provider defer-wait sample count = %d, want 2", c)
	}
}

func TestQueueDepthAndLoopAndAborts(t *testing.T) {
	m := New()
	m.SetQueueDepth("bob", 7)
	m.SetQueueDepth("bob", 4) // gauge: latest wins
	m.IncLoopIteration("bob")
	m.IncLoopIteration("bob")
	m.IncPasteUnsafeAbort("bob", "awaiting_operator")

	if got := testutil.ToFloat64(m.queueDepth.WithLabelValues("bob")); got != 4 {
		t.Errorf("queue depth = %v, want 4 (latest)", got)
	}
	if got := testutil.ToFloat64(m.loopIterations.WithLabelValues("bob")); got != 2 {
		t.Errorf("loop iterations = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.pasteUnsafeAborts.WithLabelValues("bob", "awaiting_operator")); got != 1 {
		t.Errorf("paste-unsafe aborts = %v, want 1", got)
	}
}

func TestProviderDeferInflightGauge(t *testing.T) {
	m := New()
	m.SetProviderDeferInflight("anthropic", 2)
	m.SetProviderDeferInflight("anthropic", 1) // gauge: latest wins

	if got := testutil.ToFloat64(m.providerDeferInflight.WithLabelValues("anthropic")); got != 1 {
		t.Errorf("provider defer inflight = %v, want 1 (latest)", got)
	}
	if got := testutil.ToFloat64(m.providerDeferInflight.WithLabelValues("openai")); got != 0 {
		t.Errorf("untouched provider defer inflight = %v, want 0", got)
	}
}

func TestHandler_ServesValidExposition(t *testing.T) {
	m := New()
	m.RecordDelivery("alice", "bob", StateDelivered)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		"tmux_tell_messages_total",
		`from="alice"`,
		`to="bob"`,
		`state="delivered"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("exposition missing %q; got:\n%s", want, text)
		}
	}
}

// TestSetMailmanStuck pins the #300 gauge: Set=1 on park, Set=0 on unpark.
func TestSetMailmanStuck(t *testing.T) {
	m := New()

	// Park: gauge must be 1.
	m.SetMailmanStuck("bob", "pane-not-found", true)
	if got := testutil.ToFloat64(m.mailmanStuck.WithLabelValues("bob", "pane-not-found")); got != 1 {
		t.Errorf("stuck gauge after park = %v, want 1", got)
	}

	// Unpark: gauge must drop to 0.
	m.SetMailmanStuck("bob", "pane-not-found", false)
	if got := testutil.ToFloat64(m.mailmanStuck.WithLabelValues("bob", "pane-not-found")); got != 0 {
		t.Errorf("stuck gauge after unpark = %v, want 0", got)
	}

	// Two agents are independent.
	m.SetMailmanStuck("alice", "pane-not-found", true)
	if got := testutil.ToFloat64(m.mailmanStuck.WithLabelValues("bob", "pane-not-found")); got != 0 {
		t.Errorf("bob gauge changed when alice parked: %v", got)
	}
	if got := testutil.ToFloat64(m.mailmanStuck.WithLabelValues("alice", "pane-not-found")); got != 1 {
		t.Errorf("alice gauge = %v, want 1", got)
	}
}

// TestCopyModeDeferMetrics pins #526's metric surface: InitCopyModeDefer
// materializes the counter at 0 (the present-at-zero idiom from #531), Inc
// bumps it, ObserveCopyModeDeferWait records a wait sample, and series are
// per-agent independent.
func TestCopyModeDeferMetrics(t *testing.T) {
	m := New()

	// present-at-zero: the series exists at 0 before any deferral, so the
	// dashboard shows a flat 0 line rather than "no data".
	m.InitCopyModeDefer("bob")
	if got := testutil.ToFloat64(m.copymodeDefer.WithLabelValues("bob")); got != 0 {
		t.Errorf("copymode_defer after init = %v, want 0 (present-at-zero)", got)
	}

	m.IncCopyModeDefer("bob")
	m.IncCopyModeDefer("bob")
	if got := testutil.ToFloat64(m.copymodeDefer.WithLabelValues("bob")); got != 2 {
		t.Errorf("copymode_defer after 2 inc = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.copymodeDefer.WithLabelValues("alice")); got != 0 {
		t.Errorf("untouched agent copymode_defer = %v, want 0", got)
	}

	m.ObserveCopyModeDeferWait("bob", 3.5)
	if got := testutil.CollectAndCount(m.copymodeDeferWait); got == 0 {
		t.Error("copymode_defer_wait histogram has no series after Observe")
	}
}

// TestNilMetrics_AllNoOp pins the load-bearing nil-safety ergonomic: a
// disabled mailman holds a nil *Metrics and calls every method without a
// guard. None may panic, and the nil Handler must 503 rather than crash.
func TestNilMetrics_AllNoOp(t *testing.T) {
	var m *Metrics // nil
	m.RecordDelivery("a", "b", StateDelivered)
	m.ObserveDeliveryLatency("b", 1)
	m.ObserveVerifyAttempt("b", 1)
	m.SetQueueDepth("b", 3)
	m.IncLoopIteration("b")
	m.IncPasteUnsafeAbort("b", "unknown")
	m.SetMailmanStuck("b", "pane-not-found", true)
	m.IncProviderDefer("anthropic")
	m.SetProviderDeferInflight("anthropic", 1)
	m.ObserveProviderDeferWait("anthropic", 1)
	m.InitCopyModeDefer("b")
	m.IncCopyModeDefer("b")
	m.ObserveCopyModeDeferWait("b", 1)
	if m.Registry() != nil {
		t.Error("nil Metrics Registry() should be nil")
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil Handler status = %d, want 503", rec.Code)
	}
}

// TestNew_PrivateRegistriesDoNotCollide guards the test-relevant property
// that two mailmen (or two tests) in one process each get their own
// registry — New must never touch the global default registerer.
func TestNew_PrivateRegistriesDoNotCollide(t *testing.T) {
	m1 := New()
	m2 := New()
	m1.RecordDelivery("a", "b", StateDelivered)

	if got := testutil.ToFloat64(m1.messagesTotal.WithLabelValues("a", "b", StateDelivered)); got != 1 {
		t.Errorf("m1 = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m2.messagesTotal.WithLabelValues("a", "b", StateDelivered)); got != 0 {
		t.Errorf("m2 should be independent = %v, want 0", got)
	}
}
