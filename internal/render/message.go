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
// When the body exceeds byteMarkerThreshold, a compact length marker is
// appended to the header (#160) — `· 2.3k` — so a reader scrolling
// history can distinguish a two-line ack from a 3K wall of review text,
// and a sender sees the size cost of what they're about to send:
//
//	[Surveyor → Quartermaster · re abad · id 4825 · 2.3k]
//
// A threshold < 0 disables the marker. The count is the raw body byte
// length, formatted via formatBytes.
//
// The bracket-header format (per #121) replaced the box-drawing rules
// that wrapped awkwardly on narrow viewports (mobile chat clients) and
// hit font-fallback to underline glyphs where U+2500 wasn't available.
// ASCII bracket + middle-dot + arrow render identically across all
// surfaces. Trailing footer rule dropped — the blank line between
// header and body separates the envelope label from content, and the
// bracket-open at the start of each new header delimits consecutive
// messages on visual scan.
func Message(m store.Message, byteMarkerThreshold int) string {
	var nrSuffix string
	if m.NoReplyExpected {
		nrSuffix = " · 🔕"
	}
	marker := byteMarkerSuffix(m.Body, byteMarkerThreshold)
	var header string
	clock := formatClock(m.CreatedAt)
	if m.ReplyTo.Valid && m.ReplyTo.String != "" {
		header = fmt.Sprintf("[%s → %s · re %s · id %s%s%s]",
			titleCase(m.FromAgent), titleCase(m.ToAgent),
			m.ReplyTo.String, m.PublicID, nrSuffix, marker)
	} else {
		header = fmt.Sprintf("[%s · %s · id %s%s%s]",
			titleCase(m.FromAgent), clock, m.PublicID, nrSuffix, marker)
	}
	return fmt.Sprintf("%s\n\n%s\n", header, m.Body)
}

// DefaultByteMarkerThreshold is the compile-time fallback body-byte
// cutoff for the bracket-header length marker (#160): a message whose
// body exceeds this many bytes gains a ` · <size>` suffix. Operators
// override via the render-byte-marker-threshold TOML key (fleet default
// + per-agent override). A threshold < 0 disables the marker entirely.
const DefaultByteMarkerThreshold = 512

// byteMarkerSuffix returns the ` · 2.3k` length-marker fragment for a
// body that exceeds threshold, or "" when the body is at/under threshold
// (or when threshold is negative — the explicit-disable sentinel). The
// count is the raw body byte length (len on a Go string), matching the
// issue's "body byte-count": multibyte content counts its encoded bytes,
// which is the paste-size cost that actually matters.
func byteMarkerSuffix(body string, threshold int) string {
	if threshold < 0 {
		return ""
	}
	n := len(body)
	if n <= threshold {
		return ""
	}
	return " · " + formatBytes(n)
}

// formatBytes renders a byte count in the marker's compact human form:
// `<n>b` under 1000 bytes, `<n.n>k` (×1000, one decimal) at/above. The
// 1000-base (not 1024) is deliberate: #160 pins `2.3k` == 2300 bytes, so
// an operator can map a marker back to a byte count — and a threshold
// like "2k" back to a marker — without a power-of-two conversion. The
// lowercase suffix mirrors the `du -h`/`ls -h` visual style even though
// those tools are 1024-based; the style is borrowed, the math is not.
func formatBytes(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%db", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
