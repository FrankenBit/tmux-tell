package cli

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/metrics"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// startMetricsServer brings up the Prometheus /metrics endpoint for a mailman
// run (#146). It serves m's registry on addr and shuts down when stopCtx
// cancels (mailman SIGTERM). A bind failure is logged as a WARN and the
// mailman keeps running — observability is best-effort, never load-bearing
// for delivery (fail-loud-not-fail-stop, the same stance the rest of the
// daemon takes toward non-critical subsystems). Two goroutines: one serving,
// one waiting on stopCtx to Shutdown.
func startMetricsServer(stopCtx context.Context, m *metrics.Metrics, addr string, logger *log.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		logger.Printf("metrics endpoint listening addr=%s path=/metrics", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("WARN metrics_server_err addr=%s err=%v — metrics endpoint down; delivery unaffected", addr, err)
		}
	}()

	go func() {
		<-stopCtx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
}

// pasteUnsafeReason maps a pre-paste-safety probe outcome to the stable
// `reason` label on tmux_tell_paste_unsafe_aborts_total. This counter is
// intentionally pre-paste TOCTOU scoped: it records the rare case where the
// gate passed, then the pane became paste-unsafe before paste. Steady-state
// defers/parks (rate_limited / usage_limited) are surfaced by their gauges,
// not by this counter. The label set is closed (awaiting_operator |
// compaction | rate_limited | usage_limited | copy_mode | errored | unknown |
// probe_failed) so the
// Grafana panel can enumerate it — it mirrors #146's exposition sketch and
// the IsPasteUnsafe state set (tmuxio.state). A probe error outranks the
// state classification because a failed probe already coerces the state to
// StateUnknown; the explicit probe_failed label keeps the two distinguishable
// for the operator reading the heatmap.
func pasteUnsafeReason(state tmuxio.State, probeErr error) string {
	if probeErr != nil {
		return "probe_failed"
	}
	switch state {
	case tmuxio.StateAwaitingOperator:
		return "awaiting_operator"
	case tmuxio.StateAtRestInCompaction:
		return "compaction"
	case tmuxio.StateRateLimited:
		return "rate_limited"
	case tmuxio.StateUsageLimited:
		return "usage_limited"
	case tmuxio.StateInCopyMode:
		return "copy_mode"
	case tmuxio.StateErrored:
		return "errored"
	default:
		return "unknown"
	}
}
