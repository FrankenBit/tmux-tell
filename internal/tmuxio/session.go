package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// PaneCurrentPath returns tmux's current working directory for pane. The value
// is later used as a shell `cd` target, so callers must quote it at the shell
// boundary rather than trusting tmux content to be shell-safe.
func PaneCurrentPath(ctx context.Context, pane string) (string, error) {
	if pane == "" {
		return "", errors.New("tmuxio: pane required")
	}
	out, err := tmuxRun(ctx, nil, "display-message", "-p", "-t", pane, "#{pane_current_path}")
	if err != nil {
		return "", fmt.Errorf("tmuxio: display-message pane_current_path: %w: %s", err, strings.TrimSpace(string(out)))
	}
	path := strings.TrimRight(string(out), "\r\n")
	if path == "" {
		return "", errors.New("tmuxio: pane current path empty")
	}
	return path, nil
}

// PaneCurrentCommand returns the command name of the process currently in the
// foreground of pane (tmux's `#{pane_current_command}`) — e.g. "claude",
// "codex", "node", or a shell like "bash" once the adapter has exited. Unlike a
// capture-pane/PS1 read this is adapter-agnostic and needs no host-shell-specific
// prompt parsing: it is the #285/#730 "has the adapter exited to a shell?" probe.
// An empty result (no error) means tmux reported no current command (a dead or
// transitional pane); callers treat that as "not a shell" (not yet safe to
// relaunch) rather than an error.
func PaneCurrentCommand(ctx context.Context, pane string) (string, error) {
	if pane == "" {
		return "", errors.New("tmuxio: pane required")
	}
	out, err := tmuxRun(ctx, nil, "display-message", "-p", "-t", pane, "#{pane_current_command}")
	if err != nil {
		return "", fmt.Errorf("tmuxio: display-message pane_current_command: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// shellProcessNames is the set of interactive-shell command names a chamber pane
// falls back to once its adapter process exits. tmux reports login shells
// (`exec -l /bin/bash`) with the leading '-' stripped, so the bare names suffice.
var shellProcessNames = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "fish": true,
	"dash": true, "ksh": true, "ash": true, "tcsh": true, "csh": true,
}

// IsShellProcess reports whether cmd (a PaneCurrentCommand value) is an
// interactive shell — i.e. the adapter has exited and the pane is back at a bare
// shell prompt, ready for a `send-keys <relaunch_cmd>` restart. Positively
// matching the small, stable set of shells is more robust than trying to
// enumerate every adapter/runtime process name (claude, codex, node, …).
func IsShellProcess(cmd string) bool {
	return shellProcessNames[strings.TrimPrefix(cmd, "-")]
}

// RespawnPane replaces pane with command via `tmux respawn-pane -k`. tmux takes
// command as a shell string; callers own quoting of any interpolated values.
func RespawnPane(ctx context.Context, pane, command string) error {
	if pane == "" {
		return errors.New("tmuxio: pane required")
	}
	if command == "" {
		return errors.New("tmuxio: respawn command required")
	}
	if out, err := tmuxRun(ctx, nil, "respawn-pane", "-k", "-t", pane, command); err != nil {
		return fmt.Errorf("tmuxio: respawn-pane: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RespawnPaneOriginal (retired, #285/#730) used `tmux respawn-pane -k` with NO
// command to re-run the pane's ORIGINAL creation command. Its load-bearing
// assumption — that the original command is the memory-cap wrapper — is false
// under tmux-resurrect, where pane_start_command is the resurrect restore
// (`cat …; exec -l /bin/bash`), so respawn-pane -k produced a BARE SHELL, never
// the chamber (root cause of the #285 respawn-bailout incident). The restart path
// now send-keys a registered relaunch_cmd into the post-exit shell instead; see
// internal/cli/respawn.go relaunchAfterExit. Removed rather than deprecated —
// respawnChamber was its only caller.
