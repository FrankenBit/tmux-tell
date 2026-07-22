package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAddReaped_CountsByReasonAndIsNoOpOnNonPositive(t *testing.T) {
	m := New()
	m.AddReaped("recipient-unreachable", 3)
	m.AddReaped("recipient-unreachable", 2)
	if got := testutil.ToFloat64(m.reapedTotal.WithLabelValues("recipient-unreachable")); got != 5 {
		t.Errorf("reaped_total{recipient-unreachable} = %v, want 5", got)
	}
	// A zero-reap sweep must not touch the series; a negative n is defensive.
	m.AddReaped("recipient-unreachable", 0)
	m.AddReaped("recipient-unreachable", -4)
	if got := testutil.ToFloat64(m.reapedTotal.WithLabelValues("recipient-unreachable")); got != 5 {
		t.Errorf("after no-op adds = %v, want 5 (unchanged)", got)
	}
	// Nil-safe: a disabled-metrics observer passes nil.
	var nilM *Metrics
	nilM.AddReaped("recipient-unreachable", 1) // must not panic
}
