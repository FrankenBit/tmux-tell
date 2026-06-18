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
// `reason` label on tmux_tell_paste_unsafe_aborts_total. The label set is
// closed (awaiting_operator | compaction | copy_mode | unknown | probe_failed)
// so the Grafana panel can enumerate it — it mirrors #146's exposition sketch
// and the IsPasteUnsafe state set (tmuxio.state). A probe error outranks the
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
	case tmuxio.StateInCopyMode:
		return "copy_mode"
	default:
		return "unknown"
	}
}
