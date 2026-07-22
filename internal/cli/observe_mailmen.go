package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/metrics"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

const defaultMailmanObserveInterval = 30 * time.Second

type mailmanObservation struct {
	active, enabled bool
	restarts        int
	result          string
}

type mailmanEpisode struct {
	inactiveSamples int
	restartStable   int
	lastRestarts    int
	alerted         bool
}

// autoReapConfig is the resolved #836 auto-reap policy for the observer.
type autoReapConfig struct {
	enabled   bool
	interval  time.Duration
	olderThan string
}

func resolveAutoReap(cfg *config.File) autoReapConfig {
	return autoReapConfig{
		enabled:   config.ResolveBool(cfg, "", "auto-reap-enabled", false),
		interval:  config.ResolveDuration(cfg, "", "auto-reap-interval", config.DefaultAutoReapInterval),
		olderThan: config.ResolveString(cfg, "", "auto-reap-older-than", config.DefaultAutoReapOlderThan),
	}
}

// reapReasonUnreachable is the low-cardinality metric label for the current (and
// only) reap reason: the recipient is unreachable (unregistered or pane-less).
const reapReasonUnreachable = "recipient-unreachable"

func runObserveMailmenCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("observe-mailmen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	alertTo := fs.String("alert-to", "", "conductor that receives alerts (default: mailman-alert-to TOML)")
	interval := fs.Duration("interval", defaultMailmanObserveInterval, "systemd sweep interval")
	metricsAddr := fs.String("metrics-addr", "", "expose a Prometheus /metrics endpoint on this address (e.g. ':9098'); empty disables it. TOML: observer-metrics-addr (#836)")
	once := fs.Bool("once", false, "run one sweep and exit (diagnostic; never alerts on a first inactive sample)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil || fs.NArg() != 0 || *interval <= 0 {
		return exitUsage
	}
	cfg, err := config.Load()
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("load config: %v", err), exitInternal)
	}
	if *alertTo == "" {
		*alertTo = config.ResolveString(cfg, "", "mailman-alert-to", "")
	}
	if *metricsAddr == "" {
		*metricsAddr = config.ResolveString(cfg, "", "observer-metrics-addr", "")
	}
	reap := resolveAutoReap(cfg)

	// The observer has work when EITHER it can alert a conductor OR auto-reap is
	// enabled — the two jobs are orthogonal (alerting on dead mailmen vs reaping
	// dead fossils), so auto-reap must NOT inherit the alert gate. It is dormant
	// only when both are off, and then waits (reloading config) until one turns
	// on.
	if *alertTo == "" && !reap.enabled {
		if *once {
			fmt.Fprintln(stderr, "observe-mailmen: dormant (set mailman-alert-to or auto-reap-enabled in [defaults])")
			return exitOK
		}
		fmt.Fprintln(stderr, "observe-mailmen: dormant; waiting for mailman-alert-to or auto-reap-enabled in [defaults]")
		for *alertTo == "" && !reap.enabled {
			time.Sleep(*interval)
			cfg, err = config.Load()
			if err != nil {
				fmt.Fprintf(stderr, "WARN mailman_observer_config_reload_failed err=%v\n", err)
				continue
			}
			*alertTo = config.ResolveString(cfg, "", "mailman-alert-to", "")
			reap = resolveAutoReap(cfg)
		}
	}
	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()

	var m *metrics.Metrics
	if *metricsAddr != "" {
		m = metrics.New()
		startMetricsServer(ctx, m, *metricsAddr, log.New(stderr, "", log.LstdFlags))
	}

	episodes := map[string]*mailmanEpisode{}
	var lastReap time.Time // zero → first eligible tick reaps immediately
	for {
		now := time.Now()
		if *alertTo != "" {
			if err := observeMailmenSweep(ctx, s, *alertTo, episodes, stderr); err != nil {
				fmt.Fprintf(stderr, "WARN mailman_observer_sweep_failed err=%v\n", err)
			}
		}
		if dueForReap(reap.enabled, lastReap, now, reap.interval) {
			if _, err := observerReapSweep(ctx, s, reap.olderThan, now, m, stderr); err != nil {
				fmt.Fprintf(stderr, "WARN auto_reap_failed err=%v\n", err)
			}
			lastReap = now
		}
		if *once {
			return exitOK
		}
		time.Sleep(*interval)
	}
}

// dueForReap decides whether the observer runs an auto-reap pass this tick: only
// when auto-reap is enabled AND at least `interval` has elapsed since the last
// pass. Gating the whole decision on `enabled` here is the single opt-in point —
// flipping it false disables the sweep entirely (#836).
func dueForReap(enabled bool, last, now time.Time, interval time.Duration) bool {
	return enabled && now.Sub(last) >= interval
}

// observerReapSweep runs one fleet-wide auto-reap pass: it dead-letters every
// undeliverable queued fossil older than olderThan across ALL recipients
// (agent="" — the observer is fleet-scoped, unlike the per-agent retention
// sweep, which cannot reach a dead recipient's rows) and adds the count to the
// reaped counter. Returns the number reaped.
func observerReapSweep(ctx context.Context, s *store.Store, olderThan string, now time.Time, m *metrics.Metrics, logw io.Writer) (int64, error) {
	w, err := parseWindow(olderThan, now)
	if err != nil {
		return 0, fmt.Errorf("auto-reap: parse older-than %q: %w", olderThan, err)
	}
	if w.All {
		return 0, fmt.Errorf("auto-reap: older-than %q resolves to 'all'; refusing to reap every queued row", olderThan)
	}
	cutoff := w.Since.UTC().Format(strandedTimeFormat)
	reason := fmt.Sprintf("dead-letter-reap: recipient unreachable, queued >%s, never claimed (#726/#836 auto-reap)", olderThan)
	n, err := s.ReapUndeliverable(ctx, "", cutoff, reason)
	if err != nil {
		return 0, fmt.Errorf("auto-reap: %w", err)
	}
	if n > 0 {
		m.AddReaped(reapReasonUnreachable, int(n))
		fmt.Fprintf(logw, "auto_reap_swept reaped=%d older_than=%s cutoff=%s\n", n, olderThan, cutoff)
	}
	return n, nil
}

func observeMailmenSweep(ctx context.Context, s *store.Store, conductor string, episodes map[string]*mailmanEpisode, logw io.Writer) error {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return err
	}
	resolve := mailmanUnitResolverForStore(ctx, s)
	seen := map[string]bool{}
	for _, agent := range agents {
		seen[agent.Name] = true
		// mailbox-only and hook-context agents intentionally have no persistent
		// paste mailman; an inactive unit is healthy for them.
		if agent.DeliveryMode != "" && agent.DeliveryMode != "paste-and-enter" {
			delete(episodes, agent.Name)
			continue
		}
		obs, err := inspectMailmanUnit(ctx, resolve(agent.Name))
		if err != nil {
			fmt.Fprintf(logw, "WARN mailman_observer_probe_failed agent=%s err=%v\n", agent.Name, err)
			continue
		}
		ep := episodes[agent.Name]
		if ep == nil {
			ep = &mailmanEpisode{lastRestarts: obs.restarts}
			episodes[agent.Name] = ep
		}
		if !obs.enabled { // explicit disable is an operator decision, not a death
			*ep = mailmanEpisode{lastRestarts: obs.restarts}
			continue
		}
		reason := ""
		if !obs.active {
			ep.restartStable = 0
			ep.inactiveSamples++
			if ep.inactiveSamples >= 2 {
				reason = fmt.Sprintf("unit inactive for %d consecutive sweeps (result=%s)", ep.inactiveSamples, obs.result)
			}
		} else {
			wasInactive := ep.inactiveSamples > 0
			ep.inactiveSamples = 0
			restartDelta := obs.restarts - ep.lastRestarts
			if wasInactive {
				// An inactive episode has visibly recovered.
				ep.alerted = false
			}
			if restartDelta == 0 {
				ep.restartStable++
				// A restart-loop episode recovers only after two stable samples;
				// transient active windows between crashes must not re-arm alerts.
				if ep.restartStable >= 2 {
					ep.alerted = false
				}
			} else {
				ep.restartStable = 0
			}
			if restartDelta >= 3 {
				reason = fmt.Sprintf("restart loop: NRestarts increased from %d to %d", ep.lastRestarts, obs.restarts)
			}
		}
		if reason == "" {
			ep.lastRestarts = obs.restarts
			continue
		}
		if !ep.alerted {
			body := fmt.Sprintf(":warning: Mailman for %s is not healthy — %s. Check `systemctl --user status %s` and the user journal.", agent.Name, reason, resolve(agent.Name))
			res, insertErr := s.InsertNotice(ctx, store.InsertParams{FromAgent: agent.Name, ToAgent: conductor, Body: body, Kind: store.KindDeadMailmanNotice})
			if insertErr != nil {
				return fmt.Errorf("alert %s: %w", agent.Name, insertErr)
			}
			fmt.Fprintf(logw, "dead_mailman_notice_sent agent=%s to=%s id=%s reason=%q\n", agent.Name, conductor, res.PublicID, reason)
			ep.alerted = true
		}
		ep.lastRestarts = obs.restarts
	}
	for name := range episodes {
		if !seen[name] {
			delete(episodes, name)
		}
	}
	return nil
}

func inspectMailmanUnit(ctx context.Context, unit string) (mailmanObservation, error) {
	out, err := systemctlRun(ctx, "show", unit, "--property=ActiveState", "--property=UnitFileState", "--property=NRestarts", "--property=Result")
	if err != nil {
		return mailmanObservation{}, fmt.Errorf("systemctl show: %w: %s", err, strings.TrimSpace(string(out)))
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = v
		}
	}
	restarts, _ := strconv.Atoi(props["NRestarts"])
	enabled := props["UnitFileState"] == "enabled" || props["UnitFileState"] == "enabled-runtime"
	return mailmanObservation{active: props["ActiveState"] == "active", enabled: enabled, restarts: restarts, result: props["Result"]}, nil
}
