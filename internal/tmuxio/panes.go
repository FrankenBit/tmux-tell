// Package tmuxio wraps the small set of tmux operations cli-semaphore
// needs. The package isolates every shell-out to `tmux` so the rest of the
// code is testable without a running tmux server.
//
// This file holds the read-only liveness check used by the agents/status
// subcommands. The write side (load-buffer + paste-buffer + send-keys) is
// added by issue #5.
package tmuxio

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
)

// listPanesRunner is the shell-out used by LivePanes. Tests swap it for a
// table-driven fake; production code goes through `tmux list-panes`.
var listPanesRunner = func(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tmux", "list-panes", "-aF", "#{pane_id}")
	return cmd.Output()
}

// LivePanes returns the set of pane ids (e.g. "%1", "%3") currently
// attached to the tmux server. If tmux isn't running (no server, or the
// command exits non-zero), the result is an empty set without error — the
// caller treats every stored pane_id as stale.
func LivePanes(ctx context.Context) (map[string]bool, error) {
	out, err := listPanesRunner(ctx)
	if err != nil {
		// Likely "no server running" — not an exceptional condition for
		// callers that just want to filter live vs stale.
		var ee *exec.ExitError
		if asExitError(err, &ee) {
			return map[string]bool{}, nil
		}
		// command-not-found (no tmux installed) we still surface as empty
		// rather than error — installing tmux is an operator concern, not
		// a runtime one.
		if isExecNotFound(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	live := map[string]bool{}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte{'\n'}) {
		s := strings.TrimSpace(string(line))
		if s != "" {
			live[s] = true
		}
	}
	return live, nil
}

func asExitError(err error, out **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if !ok {
		return false
	}
	*out = ee
	return true
}

func isExecNotFound(err error) bool {
	_, ok := err.(*exec.Error)
	return ok
}
