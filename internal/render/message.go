// Package render turns a store.Message into the human-facing block that
// gets pasted into the recipient's tmux pane. Pure functions; no I/O. The
// mailman and the M5 `log --thread` subcommand share these.
package render

import (
	"fmt"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// HeaderRule is the divider line that brackets a rendered message. 48 cols
// fits in most operator terminals without wrapping and is wide enough to
// read at a glance.
const headerRule = "────────────────────────────────────────────────"

// titleCase returns "Bosun" given "bosun" — for the header, since the
// stored agent names are lowercase by convention.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// formatClock returns "HH:MM:SS" in the host's local timezone from an
// ISO 8601 UTC timestamp string. Returns "??:??:??" if the input doesn't
// parse — we'd rather render with a placeholder than block delivery on a
// format mismatch.
//
// Local-time (not UTC) since 2026-06-01: the rendered header is
// operator-facing convenience and should be wall-clock-comparable.
// journalctl logs already use local time; this keeps the two correlated.
// The stored CreatedAt remains ISO 8601 UTC — only the rendered
// presentation is local. Cross-timezone unambiguity is not a concern in
// the single-host single-operator deployment shape.
func formatClock(iso string) string {
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
	} {
		if t, err := time.Parse(layout, iso); err == nil {
			return t.Local().Format("15:04:05")
		}
	}
	return "??:??:??"
}

// Message renders a single message as the recipient should see it.
// Format depends on whether the message has a reply_to (see README).
//
// Regular message:
//
//	─── Message from Bosun ── 11:04:12 ── id 7f3a ──
//	<body>
//	────────────────────────────────────────────────
//
// Reply (the to_agent is the operator-visible recipient, but the header
// shows the original sender for clarity):
//
//	─── Reply from Surveyor → Bosun ── re: 7f3a ── id 9c1d ──
//	<body>
//	────────────────────────────────────────────────
func Message(m store.Message) string {
	var header string
	clock := formatClock(m.CreatedAt)
	if m.ReplyTo.Valid && m.ReplyTo.String != "" {
		header = fmt.Sprintf("─── Reply from %s → %s ── re: %s ── id %s ──",
			titleCase(m.FromAgent), titleCase(m.ToAgent),
			m.ReplyTo.String, m.PublicID)
	} else {
		header = fmt.Sprintf("─── Message from %s ── %s ── id %s ──",
			titleCase(m.FromAgent), clock, m.PublicID)
	}
	return fmt.Sprintf("%s\n%s\n%s\n", header, m.Body, headerRule)
}
