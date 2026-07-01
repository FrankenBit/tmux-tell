package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// setRespawnAfterShrinksResult is the shared wire shape for the
// `set-respawn-after-shrinks` CLI subcommand (#285). Mirrors setMetabolismResult
// so a future MCP surface can stay byte-identical (JSON tags = source of truth).
type setRespawnAfterShrinksResult struct {
	OK    bool   `json:"ok"`
	Agent string `json:"agent"` // the target chamber configured
	// RespawnAfterShrinks is the applied threshold N: the mailman respawns the
	// target after N counted context-shrink events. 0 = respawn disabled.
	RespawnAfterShrinks int `json:"respawn_after_shrinks"`
}

// setRespawnAfterShrinks is the shared core behind #285's per-chamber respawn
// threshold config. Unlike set-metabolism (a self-only signal a chamber owns),
// this is operator/orchestrator config that legitimately TARGETS a chamber — the
// operator tunes Pilot (@200K window) independently of Engineer (@1M). Validation
// (n >= 0) lives in store.SetRespawnAfterShrinks, the single validation site.
func setRespawnAfterShrinks(ctx context.Context, s *store.Store, target string, n int) (setRespawnAfterShrinksResult, error) {
	if target == "" {
		return setRespawnAfterShrinksResult{}, errors.New(
			"no target chamber — pass --name <chamber> (or run inside a registered pane to self-target)")
	}
	if err := s.SetRespawnAfterShrinks(ctx, target, n); err != nil {
		return setRespawnAfterShrinksResult{}, err
	}
	return setRespawnAfterShrinksResult{OK: true, Agent: target, RespawnAfterShrinks: n}, nil
}

// runSetRespawnAfterShrinksCLI parses the `set-respawn-after-shrinks` subcommand
// (#285): enables (or disables, N=0) bounded post-shrink respawn for a target
// chamber. The running mailman reads the persisted threshold from the agent row
// each delivery cycle, so a change takes effect on the NEXT cycle WITHOUT a
// mailman restart — the ergonomic that fits opt-in-after-observation (the
// operator watches a chamber's heap grow via alcatraz-infra#33, then opts it in).
//
// Usage:
//
//	tmux-tell-claude set-respawn-after-shrinks --name pilot 3
//	tmux-tell-claude set-respawn-after-shrinks --name pilot 0   # disable
//	tmux-tell-claude set-respawn-after-shrinks 3                # self-target
func runSetRespawnAfterShrinksCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("set-respawn-after-shrinks", flag.ContinueOnError)
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
			"usage: %s set-respawn-after-shrinks [--name <chamber>] <N>   (N >= 0; 0 disables)\n",
			active.BinaryName)
		return exitUsage
	}
	n, err := strconv.Atoi(valueArg)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("invalid N %q: must be a non-negative integer", valueArg), exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	target := strings.TrimSpace(*name)
	if target == "" {
		// No explicit target: self-target from the current pane (a chamber
		// tuning its own threshold). Best-effort — if it can't resolve, the
		// empty-target error in setRespawnAfterShrinks tells the caller to
		// pass --name.
		if self, rerr := resolveMCPIdentity(context.Background(), s); rerr == nil {
			target = self
		}
	}
	res, err := setRespawnAfterShrinks(context.Background(), s, target, n)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	switch *format {
	case "json":
		_ = writeJSONResult(stdout, res)
	case "text", "":
		if res.RespawnAfterShrinks == 0 {
			fmt.Fprintf(stdout, "respawn disabled for %s (threshold 0)\n", res.Agent)
		} else {
			fmt.Fprintf(stdout, "respawn-after-shrinks set to %d for %s\n", res.RespawnAfterShrinks, res.Agent)
		}
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
	return exitOK
}
