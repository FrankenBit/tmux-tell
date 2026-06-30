package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// maybeAutoClearMetabolism implements the #621 observed-supersedes-self-report
// rule: a compact-pending self-report auto-clears once the mailman OBSERVES the
// chamber actually at-rest-in-compaction. It clears for that observed state and
// no other (the store guards the clear to compact-pending, so warming/saturating
// self-reports survive). Extracted from the serve observe loop so the decision
// is unit-testable across every observed state WITHOUT faking the live-elapsed
// compaction pane probe (the hardest state to synthesize).
func maybeAutoClearMetabolism(ctx context.Context, s *store.Store, agent string, observed tmuxio.State) error {
	if observed != tmuxio.StateAtRestInCompaction {
		return nil
	}
	return s.ClearMetabolismIfPending(ctx, agent)
}

// setMetabolismResult is the shared wire shape for the `set_metabolism` MCP tool
// and the `set-metabolism` CLI subcommand — both go through setMetabolism so the
// two surfaces stay byte-identical (the JSON tags are the single source of truth,
// same discipline as setPaneNameResult / pingResult / agentState).
type setMetabolismResult struct {
	OK    bool   `json:"ok"`
	Agent string `json:"agent"` // the resolved caller — always self (#621 AC#2)
	// Metabolism is the value applied: one of warming / saturating /
	// compact-pending, or "" when the self-report was cleared.
	Metabolism string `json:"metabolism"`
	// MetabolismSetAt is the stamp the store recorded for a non-empty value;
	// empty (omitted) on the clear path.
	MetabolismSetAt string `json:"metabolism_set_at,omitempty"`
}

// setMetabolism is the shared core behind #621: a chamber self-reports its
// metabolism (warming / saturating / compact-pending), or clears it (value "").
//
// SELF-ONLY by construction. `caller` is the resolved self-identity
// (resolveMCPIdentity on both surfaces), and there is deliberately NO target
// parameter: a chamber can only set its OWN metabolism. A third-party write
// would clobber the target's real self-reported signal — the failure #621 AC#2
// guards against. This is a STRONGER guarantee than control.go:160's runtime
// ScopeSelf check (which exists only because `control` legitimately targets
// other agents in its non-resume paths); here the peer-target simply does not
// exist in the API surface, so there is no check to forget.
//
// Validation (value ∈ {3 states, empty}) lives in store.SetMetabolism — the
// single validation site — so the CLI, the MCP tool, and any future caller can
// never diverge on what is settable.
func setMetabolism(ctx context.Context, s *store.Store, caller, value string) (setMetabolismResult, error) {
	value = strings.TrimSpace(value)
	if caller == "" {
		return setMetabolismResult{}, errors.New("cannot resolve caller identity — run inside a registered tmux pane")
	}
	if err := s.SetMetabolism(ctx, caller, value); err != nil {
		return setMetabolismResult{}, err
	}
	res := setMetabolismResult{OK: true, Agent: caller, Metabolism: value}
	// Re-read the stored stamp so the result reflects exactly what was persisted
	// (and is empty on the clear path), rather than re-deriving the timestamp on
	// the caller side where it could drift from the store's clock.
	if a, err := s.GetAgent(ctx, caller); err == nil {
		res.MetabolismSetAt = a.MetabolismSetAt
	}
	return res, nil
}

// runSetMetabolismCLI parses the `set-metabolism` subcommand and self-reports
// the calling chamber's metabolism. Available on both adapters via the shared
// dispatch. Self-only: there is no `--as` (unlike set-pane-name) — a chamber's
// metabolism is intrinsically its own to report.
//
// Usage:
//
//	tmux-tell-claude set-metabolism <warming|saturating|compact-pending>
//	tmux-tell-claude set-metabolism --clear
func runSetMetabolismCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("set-metabolism", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	format := fs.String("format", "text", "text|json")
	clear := fs.Bool("clear", false, "clear this chamber's metabolism self-report")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	value := strings.TrimSpace(strings.Join(fs.Args(), " "))
	switch {
	case *clear:
		value = ""
	case value == "":
		fmt.Fprintf(stderr,
			"usage: %s set-metabolism <warming|saturating|compact-pending> (or --clear)\n",
			active.BinaryName)
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	caller, err := resolveMCPIdentity(context.Background(), s)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	res, err := setMetabolism(context.Background(), s, caller, value)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	switch *format {
	case "json":
		_ = writeJSONResult(stdout, res)
	case "text", "":
		if res.Metabolism == "" {
			fmt.Fprintf(stdout, "metabolism cleared for %s\n", res.Agent)
		} else {
			fmt.Fprintf(stdout, "metabolism set to %q for %s\n", res.Metabolism, res.Agent)
		}
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
	return exitOK
}
