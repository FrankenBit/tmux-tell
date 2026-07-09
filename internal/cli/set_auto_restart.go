package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// setAutoRestartResult is the shared wire shape for the `set-auto-restart` CLI
// subcommand (#730). Mirrors setRespawnAfterShrinksResult so a future MCP surface
// can stay byte-identical (JSON tags = source of truth).
type setAutoRestartResult struct {
	OK    bool   `json:"ok"`
	Agent string `json:"agent"` // the target chamber configured
	// AutoRestart is the applied flag: when true, a tmux-tell-triggered /compact
	// that exits the chamber is auto-relaunched via the registered relaunch_cmd.
	AutoRestart bool `json:"auto_restart"`
}

// setAutoRestart is the shared core behind #730's per-chamber auto-restart flag.
// Operator/orchestrator config that legitimately TARGETS a chamber (mirrors
// set-respawn-after-shrinks).
func setAutoRestart(ctx context.Context, s *store.Store, target string, on bool) (setAutoRestartResult, error) {
	if target == "" {
		return setAutoRestartResult{}, errors.New(
			"no target chamber — pass --name <chamber> (or run inside a registered pane to self-target)")
	}
	if err := s.SetAutoRestart(ctx, target, on); err != nil {
		return setAutoRestartResult{}, err
	}
	return setAutoRestartResult{OK: true, Agent: target, AutoRestart: on}, nil
}

// parseOnOff maps the on/off value argument to a bool. Accepts on|true|1 and
// off|false|0 (case-insensitive); anything else is an error the caller surfaces.
func parseOnOff(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "1", "yes", "enable", "enabled":
		return true, nil
	case "off", "false", "0", "no", "disable", "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("invalid value %q: want on|off", v)
	}
}

// runSetAutoRestartCLI parses the `set-auto-restart` subcommand (#730): enables or
// disables auto-restart-after-triggered-/compact for a target chamber. Requires a
// registered relaunch_cmd to actually relaunch (the flag alone only ARMS the
// co-trigger; a fire with an empty relaunch_cmd logs+skips). The running mailman
// reads the persisted flag from the agent row each delivery cycle, so a change
// takes effect on the NEXT cycle WITHOUT a mailman restart.
//
// Usage:
//
//	tmux-tell-claude set-auto-restart --name bosun on
//	tmux-tell-claude set-auto-restart --name bosun off
//	tmux-tell-claude set-auto-restart on                # self-target
func runSetAutoRestartCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("set-auto-restart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	format := fs.String("format", "text", "text|json")
	name := fs.String("name", "", "target chamber to configure (default: self, resolved from the current pane)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	valueArg := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if valueArg == "" {
		fmt.Fprintf(stderr,
			"usage: %s set-auto-restart [--name <chamber>] <on|off>\n",
			active.BinaryName)
		return exitUsage
	}
	on, perr := parseOnOff(valueArg)
	if perr != nil {
		return writeJSONError(stdout, stderr, perr.Error(), exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	target := strings.TrimSpace(*name)
	if target == "" {
		if self, rerr := resolveMCPIdentity(context.Background(), s); rerr == nil {
			target = self
		}
	}
	res, err := setAutoRestart(context.Background(), s, target, on)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	switch *format {
	case "json":
		_ = writeJSONResult(stdout, res)
	case "text", "":
		state := "disabled"
		if res.AutoRestart {
			state = "enabled"
		}
		fmt.Fprintf(stdout, "auto-restart %s for %s\n", state, res.Agent)
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
	return exitOK
}
