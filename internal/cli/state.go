package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// agentStateResult is the wire-format shape that both the CLI and
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
type agentStateResult struct {
	Agent      string          `json:"agent"`
	State      string          `json:"state"`
	Evidence   tmuxio.Evidence `json:"evidence"`
	CapturedAt string          `json:"captured_at"`
}

// resolveAgentState looks up the agent's pane and probes the agent
// state. Shared between the CLI subcommand (`tmux-tell-claude state`) and
// the MCP tool (`tmux-tell.agent_state`) so both surfaces produce
// byte-identical JSON. Returns the result + any error from agent
// resolution or tmux capture.
//
// The result's State field is populated even when err != nil — the
// safer-default-on-uncertainty contract from #65's
// playbook applied at the consumer-surface layer. Callers that want
// to act on State should still check err first; consumers that just
// want to surface "what did we observe" can render Evidence.Reason
// regardless.
func resolveAgentState(ctx context.Context, s *store.Store, agent string) (agentStateResult, error) {
	res := agentStateResult{Agent: agent}
	a, err := s.GetAgent(ctx, agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			res.State = tmuxio.StateUnknown.String()
			res.Evidence = tmuxio.Evidence{Reason: fmt.Sprintf("agent %q not registered — run '%s discover'", agent, active.BinaryName)}
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
		res.Evidence = tmuxio.Evidence{Reason: fmt.Sprintf("agent %q has no pane registered — run '%s discover'", agent, active.BinaryName)}
		res.CapturedAt = nowRFC3339()
		return res, fmt.Errorf("agent %q has no pane", agent)
	}
	// Mailbox-only short-circuit (#116). A bare-shell pane registered
	// as mailbox-only doesn't have meaningful chrome states (no Claude
	// TUI to probe); the AgentState classification's marker-based
	// heuristics would always classify as Unknown. Short-circuit to
	// Idle so consumers (the mailman gate, the operator-facing state
	// probe) get a useful answer instead of "couldn't substantiate."
	// Zero capture-pane calls — no probing.
	if a.DeliveryMode == store.DeliveryModeMailboxOnly {
		res.State = tmuxio.StateIdle.String()
		res.Evidence = tmuxio.Evidence{Reason: "mailbox-only agent — no chrome to probe; idle by definition"}
		res.CapturedAt = nowRFC3339()
		return res, nil
	}
	state, ev, err := tmuxio.AgentState(ctx, a.PaneID)
	res.State = state.String()
	res.Evidence = ev
	res.CapturedAt = nowRFC3339()
	if err != nil {
		return res, fmt.Errorf("agent_state: %w", err)
	}
	return res, nil
}

// nowRFC3339 returns the current UTC time in RFC3339 format. Wrapped
// for ease of test override if needed; the tests do not currently pin
// timestamp shape, so this is just a code-organization helper.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// runStateCLI implements `tmux-tell-claude state --agent NAME` — the
// operator-facing CLI sibling to the MCP `tmux-tell.agent_state`
// tool. Both surfaces consume resolveAgentState so the JSON schema
// is identical across them.
//
// Usage: tmux-tell-claude state --agent NAME [--format text|json] [--db PATH]
func runStateCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("state", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
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
	res, resolveErr := resolveAgentState(ctx, s, agent)

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
