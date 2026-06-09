package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// parseWindow converts a `--window`/`--since` spec into a store.StatsWindow
// relative to now. Shared by `stats` (#147), `digest --since` (#161), and,
// anticipated, `tail --since` (#148).
//
// Accepted specs:
//   - ""        → default 24h (the common operator case)
//   - "all"     → no time bound (StatsWindow.All)
//   - "<N>d"    → N days (Go's time.ParseDuration has no day unit, so this is
//     handled explicitly; e.g. "7d" = 7×24h)
//   - any time.ParseDuration spec — "1h", "24h", "90m", "30s", "1h30m"
//   - calendar shortcuts (#161, evaluated in now's location):
//   - "today" / "morning" → since local midnight today
//   - "yesterday"         → since local midnight yesterday (so it spans
//     yesterday *and* today — StatsWindow has a floor, not a ceiling)
//   - "week"              → since local midnight 7 days ago
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
	if s == "now" {
		// Floor == now: nothing in the past matches. `tail` uses this as its
		// default (#148) — start live with no backfill.
		return store.StatsWindow{Since: now}, nil
	}
	// Calendar shortcuts resolve to a local-midnight floor relative to now.
	// They predate the duration parse so "today"/"week" aren't mistaken for
	// malformed durations.
	if since, ok := calendarSince(s, now); ok {
		return store.StatsWindow{Since: since}, nil
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

// calendarSince resolves a calendar shortcut to its local-midnight floor.
// The bool is false for any non-shortcut spec so the caller falls through to
// duration parsing. Midnight is computed in now's location so "today" tracks
// the operator's wall clock, not UTC.
func calendarSince(spec string, now time.Time) (time.Time, bool) {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch spec {
	case "today", "morning":
		return midnight, true
	case "yesterday":
		return midnight.AddDate(0, 0, -1), true
	case "week":
		return midnight.AddDate(0, 0, -7), true
	default:
		return time.Time{}, false
	}
}
