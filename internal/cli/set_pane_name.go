package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// setPaneNameResult is the shared wire shape for the `set_pane_name` MCP tool
// and the `set-pane-name` CLI subcommand — both go through setPaneName so the
// two surfaces stay byte-identical (the JSON tags are the single source of
// truth, same discipline as pingResult/agentState).
type setPaneNameResult struct {
	OK    bool   `json:"ok"`
	Agent string `json:"agent,omitempty"` // resolved caller; omitted when the pane isn't a registered agent
	Pane  string `json:"pane"`            // tmux pane id the title was set on (e.g. "%5")
	Title string `json:"title"`           // the display name applied
}

// setPaneName is the shared core behind #556 Path B: assert a chamber's pane
// title. It resolves the target pane, then sets the tmux pane title via
// tmuxio.SetPaneTitle. The wrapper sets the *launch* title; this covers the
// in-session rename / session-switch case the wrapper can't observe.
//
// override is the CLI `--as NAME` value (empty for the MCP self-assert path).
func setPaneName(ctx context.Context, s *store.Store, override, title string) (setPaneNameResult, error) {
	if strings.TrimSpace(title) == "" {
		return setPaneNameResult{}, errors.New("name required")
	}
	pane, agent, err := resolveCallerPane(ctx, s, override)
	if err != nil {
		return setPaneNameResult{}, err
	}
	if err := tmuxio.SetPaneTitle(ctx, pane, title); err != nil {
		return setPaneNameResult{}, err
	}
	return setPaneNameResult{OK: true, Agent: agent, Pane: pane, Title: title}, nil
}

// resolveCallerPane decides which pane the title-set targets, and which agent
// row (if any) the caller corresponds to.
//
//   - With an explicit override (CLI `--as NAME`): target THAT agent's
//     registered pane. Lets an operator script retitle any chamber by name.
//   - Otherwise (self-assert; the MCP path and the bare CLI): the pane the
//     caller is actually in is the ground truth — $TMUX_PANE. This is
//     #549-immune: even if the registry maps this pane to a stale/wrong name,
//     the title still lands on the correct pane. A codex MCP child does not
//     inherit $TMUX_PANE (#355) but resolves a name via $TMUX_AGENT_NAME, so we
//     recover its pane from the agent row.
func resolveCallerPane(ctx context.Context, s *store.Store, override string) (pane, agent string, err error) {
	name, src, rerr := identity.Resolve(ctx, s, override)
	if rerr != nil {
		return "", "", rerr
	}
	if src == identity.SourceExplicit {
		a, gerr := s.GetAgent(ctx, name)
		if gerr != nil {
			return "", name, fmt.Errorf("agent %q: %w", name, gerr)
		}
		if a.PaneID == "" {
			return "", name, fmt.Errorf("agent %q has no registered pane", name)
		}
		return a.PaneID, name, nil
	}
	pane = os.Getenv("TMUX_PANE")
	if pane == "" && name != "" {
		if a, gerr := s.GetAgent(ctx, name); gerr == nil {
			pane = a.PaneID
		}
	}
	if pane == "" {
		return "", name, errors.New(
			"cannot resolve caller pane: $TMUX_PANE is empty and no registered " +
				"pane to fall back on — run inside a tmux pane or pass --as <name>")
	}
	return pane, name, nil
}

// runSetPaneNameCLI parses the `set-pane-name` subcommand and asserts the pane
// title. Available on both adapters (tmux-tell-claude / tmux-tell-codex) via the
// shared dispatch, so an operator can retitle a pane from a script too (#556).
//
// Usage: tmux-tell-claude set-pane-name [--as NAME] [--format text|json] <name>
func runSetPaneNameCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("set-pane-name", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	asName := fs.String("as", "",
		"target a named agent's registered pane instead of the current pane")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// Join positionals so an unquoted multi-word name (set-pane-name Master Bosun)
	// works as well as a quoted one ("Master Bosun").
	name := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if name == "" {
		fmt.Fprintf(stderr, "usage: %s set-pane-name [--as NAME] <name>\n", active.BinaryName)
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	res, err := setPaneName(context.Background(), s, *asName, name)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	switch *format {
	case "json":
		_ = writeJSONResult(stdout, res)
	case "text", "":
		fmt.Fprintf(stdout, "pane %s title set to %q\n", res.Pane, res.Title)
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
	return exitOK
}
