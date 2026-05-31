package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// runPauseCLI parses pause/resume flags and dispatches. The same handler
// runs for both — `paused` tells us which to set.
//
// Usage:
//
//	claude-msg pause  AGENT | --all
//	claude-msg resume AGENT | --all
func runPauseCLI(args []string, paused bool, stdout, stderr io.Writer) int {
	verb := "pause"
	if !paused {
		verb = "resume"
	}
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	all := fs.Bool("all", false, "apply to every registered agent")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	switch {
	case *all && fs.NArg() > 0:
		return writeJSONError(stdout, stderr, "--all and a positional agent are mutually exclusive", exitUsage)
	case !*all && fs.NArg() != 1:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("usage: claude-msg %s AGENT | --all", verb), exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	target := ""
	if !*all {
		target = fs.Arg(0)
	}
	return runPauseWithStore(context.Background(), s, target, paused, *format, stdout, stderr)
}

// agentState is the per-agent result element for pause/resume responses.
type agentState struct {
	Name   string `json:"name"`
	Paused bool   `json:"paused"`
}

func runPauseWithStore(ctx context.Context, s *store.Store,
	target string, paused bool, format string,
	stdout, stderr io.Writer,
) int {
	if target == "" {
		// --all path: flip everyone, then list every touched agent's
		// new state so the operator sees per-agent confirmation.
		if _, err := s.SetPausedAll(ctx, paused); err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		agents, err := s.ListAgents(ctx)
		if err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		out := make([]agentState, 0, len(agents))
		for _, a := range agents {
			out = append(out, agentState{Name: a.Name, Paused: a.Paused})
		}
		switch format {
		case "json":
			_ = writeJSONResult(stdout, map[string]any{
				"ok":     true,
				"paused": paused,
				"agents": out,
			})
		default:
			fmt.Fprintf(stdout, "%s applied to %d agent(s):\n", action(paused), len(out))
			renderTextTable(stdout, []string{"NAME", "PAUSED"},
				agentStateRows(out))
		}
		return exitOK
	}

	if err := s.SetPaused(ctx, target, paused); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("unknown agent: %s", target), exitUnavailable)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	return writeOK(format, stdout, map[string]any{
		"ok":     true,
		"agent":  target,
		"paused": paused,
	}, fmt.Sprintf("%s %s", action(paused), target))
}

func agentStateRows(in []agentState) [][]string {
	rows := make([][]string, 0, len(in))
	for _, a := range in {
		rows = append(rows, []string{a.Name, yesNo(a.Paused)})
	}
	return rows
}

func action(paused bool) string {
	if paused {
		return "paused"
	}
	return "resumed"
}

// writeOK prints either the JSON object or a human-readable line, matching
// the operator's --format choice.
func writeOK(format string, stdout io.Writer, jsonObj map[string]any, textLine string) int {
	switch format {
	case "json":
		_ = writeJSONResult(stdout, jsonObj)
	default:
		fmt.Fprintln(stdout, textLine)
	}
	return exitOK
}
