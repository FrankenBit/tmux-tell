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
	"strconv"
	"strings"
)

// listPanesRunner is the shell-out used by LivePanes. Tests swap it for a
// table-driven fake; production code goes through `tmux list-panes`.
var listPanesRunner = func(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tmux", "list-panes", "-aF", "#{pane_id}")
	return cmd.Output()
}

// listPanesWithPIDRunner is the swappable shell-out for ListPanesWithPID.
// Output is tab-separated so titles with spaces survive intact.
var listPanesWithPIDRunner = func(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tmux", "list-panes", "-aF",
		"#{pane_id}\t#{pane_pid}\t#{pane_title}\t#{window_name}")
	return cmd.Output()
}

// SetListPanesWithPIDRunner is for tests.
func SetListPanesWithPIDRunner(r func(ctx context.Context) ([]byte, error)) func(ctx context.Context) ([]byte, error) {
	prev := listPanesWithPIDRunner
	listPanesWithPIDRunner = r
	return prev
}

// PaneInfo describes one tmux pane for discovery purposes.
type PaneInfo struct {
	ID         string // "%3"
	PID        int    // pane root process id
	Title      string // pane_title — typically what the running app sets
	WindowName string // window_name — operator-named or app-set
}

// ListPanesWithPID returns every pane with its root process pid. Same
// soft-failure semantics as LivePanes: no tmux running → empty slice.
func ListPanesWithPID(ctx context.Context) ([]PaneInfo, error) {
	out, err := listPanesWithPIDRunner(ctx)
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
	var infos []PaneInfo
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte{'\n'}) {
		s := strings.TrimRight(string(line), "\r\n")
		if strings.TrimSpace(s) == "" {
			continue
		}
		parts := strings.SplitN(s, "\t", 4)
		if len(parts) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			continue
		}
		p := PaneInfo{
			ID:  strings.TrimSpace(parts[0]),
			PID: pid,
		}
		if len(parts) >= 3 {
			p.Title = parts[2]
		}
		if len(parts) >= 4 {
			p.WindowName = parts[3]
		}
		infos = append(infos, p)
	}
	return infos, nil
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
