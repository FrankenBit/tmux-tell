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
	if err := fs.Parse(args); err != nil {
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

func runPauseWithStore(ctx context.Context, s *store.Store,
	target string, paused bool, format string,
	stdout, stderr io.Writer,
) int {
	if target == "" {
		// --all path
		n, err := s.SetPausedAll(ctx, paused)
		if err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		return writeOK(format, stdout, map[string]any{
			"ok":      true,
			"paused":  paused,
			"updated": n,
		}, fmt.Sprintf("%s applied to %d agent(s)", action(paused), n))
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
