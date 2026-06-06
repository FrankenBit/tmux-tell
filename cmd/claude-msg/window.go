package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// parseWindow converts a `--window`/`--since` spec into a store.StatsWindow
// relative to now. Shared by `stats` (#147) and, anticipated, `tail --since`
// (#148) and `digest --since` (#161).
//
// Accepted specs:
//   - ""        → default 24h (the common operator case)
//   - "all"     → no time bound (StatsWindow.All)
//   - "<N>d"    → N days (Go's time.ParseDuration has no day unit, so this is
//     handled explicitly; e.g. "7d" = 7×24h)
//   - any time.ParseDuration spec — "1h", "24h", "90m", "30s", "1h30m"
//
// A zero or negative duration is rejected (a window must look backwards).
func parseWindow(spec string, now time.Time) (store.StatsWindow, error) {
	s := strings.TrimSpace(strings.ToLower(spec))
	if s == "" {
		s = "24h"
	}
	if s == "all" {
		return store.StatsWindow{All: true}, nil
	}

	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return store.StatsWindow{}, fmt.Errorf("invalid window %q: %w", spec, err)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return store.StatsWindow{}, fmt.Errorf("invalid window %q (want 'all', '<N>d', or a duration like '1h'/'24h'): %w", spec, err)
		}
		d = parsed
	}

	if d <= 0 {
		return store.StatsWindow{}, fmt.Errorf("invalid window %q: must be a positive duration", spec)
	}
	return store.StatsWindow{Since: now.Add(-d)}, nil
}
