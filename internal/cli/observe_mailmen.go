package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
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
	lastRestarts    int
	alerted         bool
}

func runObserveMailmenCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("observe-mailmen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	alertTo := fs.String("alert-to", "", "conductor that receives alerts (default: mailman-alert-to TOML)")
	interval := fs.Duration("interval", defaultMailmanObserveInterval, "systemd sweep interval")
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
	if *alertTo == "" {
		if *once {
			fmt.Fprintln(stderr, "observe-mailmen: dormant (set mailman-alert-to in [defaults] or pass --alert-to)")
			return exitOK
		}
		fmt.Fprintln(stderr, "observe-mailmen: dormant; waiting for mailman-alert-to in [defaults]")
		for *alertTo == "" {
			time.Sleep(*interval)
			cfg, err = config.Load()
			if err != nil {
				fmt.Fprintf(stderr, "WARN mailman_observer_config_reload_failed err=%v\n", err)
				continue
			}
			*alertTo = config.ResolveString(cfg, "", "mailman-alert-to", "")
		}
	}
	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()
	episodes := map[string]*mailmanEpisode{}
	for {
		if err := observeMailmenSweep(ctx, s, *alertTo, episodes, stderr); err != nil {
			fmt.Fprintf(stderr, "WARN mailman_observer_sweep_failed err=%v\n", err)
		}
		if *once {
			return exitOK
		}
		time.Sleep(*interval)
	}
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
			ep.inactiveSamples++
			if ep.inactiveSamples >= 2 {
				reason = fmt.Sprintf("unit inactive for %d consecutive sweeps (result=%s)", ep.inactiveSamples, obs.result)
			}
		} else {
			ep.inactiveSamples = 0
			if obs.restarts-ep.lastRestarts >= 3 {
				reason = fmt.Sprintf("restart loop: NRestarts increased from %d to %d", ep.lastRestarts, obs.restarts)
			}
		}
		if reason == "" {
			if obs.active {
				ep.alerted = false
			}
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
