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

// setRelaunchCmdResult is the shared wire shape for the `set-relaunch-cmd` CLI
// subcommand (#285/#730). Mirrors setRespawnAfterShrinksResult so a future MCP
// surface can stay byte-identical (JSON tags = source of truth).
type setRelaunchCmdResult struct {
	OK    bool   `json:"ok"`
	Agent string `json:"agent"` // the target chamber configured
	// RelaunchCmd is the applied relaunch command — the exact string the mailman
	// send-keys into a post-exit bare shell to restart the chamber.
	RelaunchCmd string `json:"relaunch_cmd"`
}

// setRelaunchCmd is the shared core behind the #285/#730 per-chamber relaunch
// command config. Like set-respawn-after-shrinks (and unlike the self-only
// set-metabolism), this is operator/orchestrator config that legitimately TARGETS
// a chamber — the operator registers each chamber's actual launch command. Empty
// cmd is allowed (leaves the relaunch primitive unconfigured); validation lives in
// store.SetRelaunchCmd.
func setRelaunchCmd(ctx context.Context, s *store.Store, target, cmd string) (setRelaunchCmdResult, error) {
	if target == "" {
		return setRelaunchCmdResult{}, errors.New(
			"no target chamber — pass --name <chamber> (or run inside a registered pane to self-target)")
	}
	if err := s.SetRelaunchCmd(ctx, target, cmd); err != nil {
		return setRelaunchCmdResult{}, err
	}
	return setRelaunchCmdResult{OK: true, Agent: target, RelaunchCmd: cmd}, nil
}

// runSetRelaunchCmdCLI parses the `set-relaunch-cmd` subcommand (#285/#730): sets
// the command the mailman send-keys into a post-exit bare shell to restart the
// chamber (e.g. `chamber-claude.sh Bosun` today → `claude --resume Bosun` once the
// wrapper is deprecated — the substrate stays indifferent to its shape). The
// running mailman reads the persisted value from the agent row each delivery
// cycle, so a change takes effect on the NEXT cycle WITHOUT a mailman restart.
//
// The command may contain its own flags/spaces; flag parsing stops at the first
// non-flag token, so everything after --name/--format/--db is captured verbatim
// (quote it to be safe):
//
//	tmux-tell-claude set-relaunch-cmd --name bosun 'chamber-claude.sh Bosun'
//	tmux-tell-claude set-relaunch-cmd --name pilot claude --resume Pilot
//	tmux-tell-claude set-relaunch-cmd 'claude --resume Engineer'   # self-target
func runSetRelaunchCmdCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("set-relaunch-cmd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	format := fs.String("format", "text", "text|json")
	name := fs.String("name", "", "target chamber to configure (default: self, resolved from the current pane)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cmd := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if cmd == "" {
		fmt.Fprintf(stderr,
			"usage: %s set-relaunch-cmd [--name <chamber>] <command...>\n",
			active.BinaryName)
		return exitUsage
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
	res, err := setRelaunchCmd(context.Background(), s, target, cmd)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	switch *format {
	case "json":
		_ = writeJSONResult(stdout, res)
	case "text", "":
		fmt.Fprintf(stdout, "relaunch-cmd set for %s: %s\n", res.Agent, res.RelaunchCmd)
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
	return exitOK
}
