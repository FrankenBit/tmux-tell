package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// chamberStateResult is the wire-format shape that both the CLI and
// the MCP tool emit. Per #69's verdict, this is the
// durable schema that Binnacle's M6b dashboard / operator API can
// consume verbatim once the bridge replaces the tmux-msg-side
// detection mechanism.
//
// Fields:
//   - Agent: which agent was probed
//   - State: the wire-format state name (matches tmuxio.State.String())
//   - Evidence: the observation that led to the classification
//   - CapturedAt: RFC3339 UTC timestamp of the probe
type chamberStateResult struct {
	Agent      string          `json:"agent"`
	State      string          `json:"state"`
	Evidence   tmuxio.Evidence `json:"evidence"`
	CapturedAt string          `json:"captured_at"`
}

// resolveChamberState looks up the agent's pane and probes the chamber
// state. Shared between the CLI subcommand (`claude-msg state`) and
// the MCP tool (`semaphore.chamber_state`) so both surfaces produce
// byte-identical JSON. Returns the result + any error from agent
// resolution or tmux capture.
//
// The result's State field is populated even when err != nil — the
// safer-default-on-uncertainty contract from #65's
// playbook applied at the consumer-surface layer. Callers that want
// to act on State should still check err first; consumers that just
// want to surface "what did we observe" can render Evidence.Reason
// regardless.
func resolveChamberState(ctx context.Context, s *store.Store, agent string) (chamberStateResult, error) {
	res := chamberStateResult{Agent: agent}
	a, err := s.GetAgent(ctx, agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			res.State = tmuxio.StateUnknown.String()
			res.Evidence = tmuxio.Evidence{Reason: fmt.Sprintf("agent %q not registered — run 'claude-msg discover'", agent)}
			res.CapturedAt = nowRFC3339()
			return res, fmt.Errorf("agent %q not registered", agent)
		}
		res.State = tmuxio.StateUnknown.String()
		res.Evidence = tmuxio.Evidence{Reason: fmt.Sprintf("get_agent error: %v", err)}
		res.CapturedAt = nowRFC3339()
		return res, fmt.Errorf("get_agent: %w", err)
	}
	if a.PaneID == "" {
		res.State = tmuxio.StateUnknown.String()
		res.Evidence = tmuxio.Evidence{Reason: fmt.Sprintf("agent %q has no pane registered — run 'claude-msg discover'", agent)}
		res.CapturedAt = nowRFC3339()
		return res, fmt.Errorf("agent %q has no pane", agent)
	}
	state, ev, err := tmuxio.ChamberState(ctx, a.PaneID)
	res.State = state.String()
	res.Evidence = ev
	res.CapturedAt = nowRFC3339()
	if err != nil {
		return res, fmt.Errorf("chamber_state: %w", err)
	}
	return res, nil
}

// nowRFC3339 returns the current UTC time in RFC3339 format. Wrapped
// for ease of test override if needed; the tests do not currently pin
// timestamp shape, so this is just a code-organization helper.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// runStateCLI implements `claude-msg state --agent NAME` — the
// operator-facing CLI sibling to the MCP `semaphore.chamber_state`
// tool. Both surfaces consume resolveChamberState so the JSON schema
// is identical across them.
//
// Usage: claude-msg state --agent NAME [--format text|json] [--db PATH]
func runStateCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("state", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	agent := fs.String("agent", "", "agent name to probe (required)")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if *agent == "" {
		return writeJSONError(stdout, stderr, "--agent required", exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer func() { _ = s.Close() }()

	return runStateWithStore(context.Background(), s, *agent, *format, stdout, stderr)
}

// runStateWithStore is the testable inner that runStateCLI calls
// after opening the store. Same pattern as runStatusWithStore.
func runStateWithStore(ctx context.Context, s *store.Store, agent, format string, stdout, stderr io.Writer) int {
	res, resolveErr := resolveChamberState(ctx, s, agent)

	switch format {
	case "json":
		// Always emit the result. Errors are surfaced via Evidence.Reason
		// so consumers can parse the JSON without guessing whether the
		// probe ran. Exit code distinguishes success from error so
		// shell scripts can branch.
		_ = writeJSONResult(stdout, res)
		if resolveErr != nil {
			return exitInternal
		}
		return exitOK
	case "text", "":
		fmt.Fprintf(stdout, "AGENT\t%s\n", res.Agent)
		fmt.Fprintf(stdout, "STATE\t%s\n", res.State)
		fmt.Fprintf(stdout, "EVIDENCE\t%s\n", res.Evidence.Reason)
		fmt.Fprintf(stdout, "CAPTURED\t%s\n", res.CapturedAt)
		if resolveErr != nil {
			// Non-zero exit so scripts know the probe failed; still print
			// the result above so the operator can see what happened.
			fmt.Fprintf(stderr, "state probe error: %v\n", resolveErr)
			return exitInternal
		}
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}
