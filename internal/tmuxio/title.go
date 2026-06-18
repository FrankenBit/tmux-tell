package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SetPaneTitle sets the tmux pane title (the `#{pane_title}` rendered in the
// status line / border) for the given pane via `tmux select-pane -T`. This is
// the single substrate primitive behind the chamber-asserted display-name
// mechanism (#556 Path B): both the `set_pane_name` MCP tool and the
// `set-pane-name` CLI subcommand funnel through here.
//
// The title is passed as one argv element, so multi-word names ("Master
// Bosun") survive intact — no quoting/splitting on the caller's part. An empty
// title is rejected: clearing a title is not a supported operation on this path
// (a chamber asserting its name always asserts a non-empty one), and an empty
// `-T ""` would silently blank the visible identity.
//
// Goes through the package-level tmuxRun indirection (shared with Deliver /
// SendKeys), so tests swap it via SetTmuxRunner without a live tmux server.
func SetPaneTitle(ctx context.Context, pane, title string) error {
	if pane == "" {
		return errors.New("tmuxio: pane required")
	}
	if title == "" {
		return errors.New("tmuxio: title required")
	}
	if out, err := tmuxRun(ctx, nil, "select-pane", "-t", pane, "-T", title); err != nil {
		return fmt.Errorf("tmuxio: select-pane -T: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
