package tmuxio

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain asserts no goroutine outlives the package's test run (#496 PR-B).
// internal/tmuxio holds the observe-gate poll loop + the deliver retry loop —
// both ctx-driven — so a test that starts one and doesn't let it return on
// cancellation would leak; goleak.VerifyTestMain catches that at the test-suite
// boundary. (The dispatch's "internal/observegate" goleak target maps here —
// observe_gate.go lives in internal/tmuxio, there is no separate package.)
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
