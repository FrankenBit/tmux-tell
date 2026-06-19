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
