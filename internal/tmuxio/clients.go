package tmuxio

import (
	"context"
	"os/exec"
	"strings"
)

// listClientsRunner is the swappable shell-out used by ActiveClientPanes.
// Tests fake it; production calls `tmux list-clients -aF '#{client_active_pane}'`.
var listClientsRunner = func(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tmux", "list-clients", "-aF", "#{client_active_pane}")
	return cmd.Output()
}

// SetListClientsRunner swaps the runner for tests, returning the previous
// runner so cleanup can restore it. Sibling to SetListPanesRunner.
func SetListClientsRunner(r func(ctx context.Context) ([]byte, error)) func(ctx context.Context) ([]byte, error) {
	prev := listClientsRunner
	listClientsRunner = r
	return prev
}

// ActiveClientPanes returns the pane ids that tmux clients are currently
// active in. Each line of `tmux list-clients -aF '#{client_active_pane}'`
// produces one entry. Soft-failure semantics: no tmux server / no tmux
// binary returns an empty slice (no clients attached).
//
// The single-operator-per-session model (#228 Q2 dissolved 2026-06-08)
// means callers typically read the first entry — but the helper returns
// the full list for substrate-honest "what tmux reports" without imposing
// a model on the caller.
func ActiveClientPanes(ctx context.Context) ([]string, error) {
	out, err := listClientsRunner(ctx)
	if err != nil {
		var ee *exec.ExitError
		if asExitError(err, &ee) {
			return nil, nil
		}
		if isExecNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	lines := strings.Split(trimmed, "\n")
	out2 := make([]string, 0, len(lines))
	for _, ln := range lines {
		if pane := strings.TrimSpace(ln); pane != "" {
			out2 = append(out2, pane)
		}
	}
	return out2, nil
}
