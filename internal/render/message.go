// Package render turns a store.Message into the human-facing block that
// gets pasted into the recipient's tmux pane. Pure functions; no I/O. The
// mailman and the M5 `log --thread` subcommand share these.
package render

import (
	"fmt"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

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
//	[Bosun · 11:04:12 · id 7f3a]
//
//	<body>
//
// Reply (the to_agent is the operator-visible recipient, but the header
// shows the original sender for clarity):
//
//	[Surveyor → Bosun · re 7f3a · id 9c1d]
//
//	<body>
//
// The bracket-header format (per #121) replaced the box-drawing rules
// that wrapped awkwardly on narrow viewports (mobile chat clients) and
// hit font-fallback to underline glyphs where U+2500 wasn't available.
// ASCII bracket + middle-dot + arrow render identically across all
// surfaces. Trailing footer rule dropped — the blank line between
// header and body separates the envelope label from content, and the
// bracket-open at the start of each new header delimits consecutive
// messages on visual scan.
func Message(m store.Message) string {
	var nrSuffix string
	if m.NoReplyExpected {
		nrSuffix = " · 🔕"
	}
	var header string
	clock := formatClock(m.CreatedAt)
	if m.ReplyTo.Valid && m.ReplyTo.String != "" {
		header = fmt.Sprintf("[%s → %s · re %s · id %s%s]",
			titleCase(m.FromAgent), titleCase(m.ToAgent),
			m.ReplyTo.String, m.PublicID, nrSuffix)
	} else {
		header = fmt.Sprintf("[%s · %s · id %s%s]",
			titleCase(m.FromAgent), clock, m.PublicID, nrSuffix)
	}
	return fmt.Sprintf("%s\n\n%s\n", header, m.Body)
}
