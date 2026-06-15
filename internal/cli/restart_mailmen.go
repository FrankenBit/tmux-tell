package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
)

// runRestartMailmenCLI implements `tmux-msg-<adapter> restart-mailmen`: restart
// every RUNNING mailman unit for the active adapter so a freshly-installed
// binary actually takes effect (#436).
//
// Why this exists: a `systemctl --user restart` is the only way an already-
// active mailman picks up a replaced binary inode — `enable --now` is a no-op
// on a running unit, so the daemon keeps the deleted-inode binary until an
// explicit restart (the #393 lesson, captured in restartMailman's doc). The
// claude install path gets this for free inside `bootstrap` (per-agent
// restartMailman). The codex `--no-bootstrap` deploy path (#436) has no
// bootstrap, so it needs a standalone primitive — this one — to make the new
// codex binary effective on Lookout's (and any) running codex mailman.
//
// Adapter-scoped by the unit-name glob (`<BinaryName>-mailman@*.service`), so
// `tmux-msg-codex restart-mailmen` only touches codex mailmen and vice versa.
// Source of truth is systemd's running-unit set, not the agents table: we
// restart what is actually alive, regardless of registry drift.
func runRestartMailmenCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restart-mailmen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	return runRestartMailmen(context.Background(), *format, stdout, stderr)
}

// runRestartMailmen enumerates the active adapter's running mailman units and
// restarts each. Returns exitOK when every restart succeeds (including the
// zero-mailmen case — nothing to do is success), exitInternal on enumeration
// failure or any restart failure (with the per-agent failures named).
func runRestartMailmen(ctx context.Context, format string, stdout, stderr io.Writer) int {
	agents, err := runningMailmanAgents(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("list mailman units: %v", err), exitInternal)
	}

	restarted := make([]string, 0, len(agents))
	failures := make(map[string]string, 0)
	for _, a := range agents {
		if rerr := restartMailman(ctx, a); rerr != nil {
			failures[a] = rerr.Error()
		} else {
			restarted = append(restarted, a)
		}
	}

	ok := len(failures) == 0
	if format == "json" {
		_ = json.NewEncoder(stdout).Encode(map[string]any{
			"ok":        ok,
			"adapter":   active.BinaryName,
			"restarted": restarted,
			"failed":    failures,
		})
	} else {
		fmt.Fprintf(stdout, "restart-mailmen (%s): %d restarted", active.BinaryName, len(restarted))
		if len(restarted) > 0 {
			fmt.Fprintf(stdout, " [%s]", strings.Join(restarted, " "))
		}
		if len(failures) > 0 {
			fmt.Fprintf(stdout, ", %d FAILED:", len(failures))
			for a, e := range failures {
				fmt.Fprintf(stdout, "\n  %s: %s", a, e)
			}
		}
		fmt.Fprintln(stdout)
	}
	if !ok {
		return exitInternal
	}
	return exitOK
}

// runningMailmanAgents returns the agent names whose active-adapter mailman unit
// is currently running, parsed from `systemctl --user list-units` over the
// adapter-scoped instance glob. Only active units are listed (--state=active),
// so a stopped/disabled mailman is correctly excluded.
func runningMailmanAgents(ctx context.Context) ([]string, error) {
	glob := active.BinaryName + "-mailman@*.service"
	out, err := systemctlRun(ctx, "list-units", "--type=service", "--state=active",
		"--plain", "--no-legend", glob)
	if err != nil {
		return nil, fmt.Errorf("systemctl list-units: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var agents []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if a := mailmanUnitAgent(fields[0]); a != "" {
			agents = append(agents, a)
		}
	}
	return agents, nil
}

// mailmanUnitAgent extracts the agent name from an active-adapter mailman unit
// name (`<BinaryName>-mailman@<agent>.service` → `<agent>`). Returns "" if the
// unit doesn't match the active adapter's template (defensive — the glob should
// already constrain it).
func mailmanUnitAgent(unit string) string {
	prefix := active.BinaryName + "-mailman@"
	rest, ok := strings.CutPrefix(unit, prefix)
	if !ok {
		return ""
	}
	return strings.TrimSuffix(rest, ".service")
}
