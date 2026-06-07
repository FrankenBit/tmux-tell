package render

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// localClockFromUTC converts an ISO 8601 UTC timestamp into the
// expected "HH:MM:SS" local-clock substring the renderer should
// produce. Lets the tests pass regardless of which timezone the
// build runs in (CI may be UTC; the alcatraz host is Europe/Berlin).
func localClockFromUTC(t *testing.T, iso string) string {
	t.Helper()
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", iso)
	if err != nil {
		parsed, err = time.Parse("2006-01-02T15:04:05Z", iso)
		if err != nil {
			t.Fatalf("parse fixture %q: %v", iso, err)
		}
	}
	return parsed.Local().Format("15:04:05")
}

func TestMessage_Regular(t *testing.T) {
	const fixtureUTC = "2026-05-29T11:04:12.000Z"
	got := Message(store.Message{
		PublicID:  "7f3a",
		FromAgent: "bosun",
		ToAgent:   "surveyor",
		Body:      "please check CI on PR 1234",
		CreatedAt: fixtureUTC,
	}, DefaultByteMarkerThreshold)
	wantSubstrings := []string{
		"[Bosun · ",
		localClockFromUTC(t, fixtureUTC),
		"id 7f3a]",
		"please check CI on PR 1234",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	// Reply-format indicators should be absent on a regular message's
	// header. Scoping to the header line (not full output) protects against
	// future fixture body content that happens to contain ` → ` or ` re `
	// triggering a false positive without indicating regular-into-reply drift.
	headerLine, _, _ := strings.Cut(got, "\n")
	if strings.Contains(headerLine, " → ") || strings.Contains(headerLine, " re ") {
		t.Errorf("regular header should not contain reply markers: %s", headerLine)
	}
}

func TestMessage_Reply(t *testing.T) {
	got := Message(store.Message{
		PublicID:  "9c1d",
		FromAgent: "surveyor",
		ToAgent:   "bosun",
		Body:      "looking now, ETA 3 min",
		ReplyTo:   sql.NullString{String: "7f3a", Valid: true},
		CreatedAt: "2026-05-29T11:05:00.000Z",
	}, DefaultByteMarkerThreshold)
	wantSubstrings := []string{
		"[Surveyor → Bosun · ",
		"re 7f3a",
		"id 9c1d]",
		"looking now, ETA 3 min",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

func TestMessage_NoReplyExpected(t *testing.T) {
	const fixtureUTC = "2026-06-07T00:00:00.000Z"
	got := Message(store.Message{
		PublicID:        "ab12",
		FromAgent:       "bosun",
		ToAgent:         "pilot",
		Body:            "FYI: tagged v0.8.0",
		CreatedAt:       fixtureUTC,
		NoReplyExpected: true,
	}, DefaultByteMarkerThreshold)
	headerLine, _, _ := strings.Cut(got, "\n")
	if !strings.Contains(headerLine, "🔕") {
		t.Errorf("no-reply header missing 🔕 marker: %s", headerLine)
	}
	if !strings.Contains(headerLine, "id ab12") {
		t.Errorf("no-reply header missing id: %s", headerLine)
	}
	// Verify regular message does NOT carry the marker.
	plain := Message(store.Message{
		PublicID:  "cd34",
		FromAgent: "bosun",
		ToAgent:   "pilot",
		Body:      "normal message",
		CreatedAt: fixtureUTC,
	}, DefaultByteMarkerThreshold)
	plainHeader, _, _ := strings.Cut(plain, "\n")
	if strings.Contains(plainHeader, "🔕") {
		t.Errorf("regular header should not contain 🔕: %s", plainHeader)
	}
}

// headerOf returns just the bracket-header line of a rendered message.
func headerOf(rendered string) string {
	h, _, _ := strings.Cut(rendered, "\n")
	return h
}

func TestMessage_ShortBodyNoMarker(t *testing.T) {
	// A body at/under the threshold must NOT gain a length marker (AC:
	// "short message renders without the marker"). The default threshold
	// is 512; this body is well under it.
	got := Message(store.Message{
		PublicID:  "aa01",
		FromAgent: "pilot",
		ToAgent:   "quartermaster",
		Body:      "ack — picking up #176 now",
		CreatedAt: "2026-06-07T09:00:00.000Z",
	}, DefaultByteMarkerThreshold)
	if header := headerOf(got); hasMarker(header) {
		t.Errorf("short body should have no length marker: %q", header)
	}
}

// hasMarker reports whether a header carries a `· <size>` length marker.
// The marker is always the last dot-separated field, starts with a digit,
// and ends in `b` or `k` — the leading-digit check guards against a
// public_id that happens to end in `b`/`k` ("id ab1k]") being misread as
// a marker.
func hasMarker(header string) bool {
	trimmed := strings.TrimSuffix(header, "]")
	fields := strings.Split(trimmed, " · ")
	last := fields[len(fields)-1]
	if last == "" || last[0] < '0' || last[0] > '9' {
		return false
	}
	return strings.HasSuffix(last, "b") || strings.HasSuffix(last, "k")
}

func TestMessage_LongBodyGetsMarker(t *testing.T) {
	// A body above the threshold gains the marker (AC: "long message
	// renders with the marker"). 2300 bytes → `2.3k` per the 1000-base
	// format the issue pins.
	body := strings.Repeat("x", 2300)
	got := Message(store.Message{
		PublicID:  "bb02",
		FromAgent: "surveyor",
		ToAgent:   "quartermaster",
		ReplyTo:   sql.NullString{String: "abad", Valid: true},
		Body:      body,
		CreatedAt: "2026-06-07T09:01:00.000Z",
	}, DefaultByteMarkerThreshold)
	header := headerOf(got)
	if !strings.Contains(header, "· 2.3k]") {
		t.Errorf("long body should carry `· 2.3k]` marker: %q", header)
	}
	// Marker sits at the end, after the id.
	if !strings.Contains(header, "id bb02 · 2.3k]") {
		t.Errorf("marker should follow the id: %q", header)
	}
}

func TestMessage_MarkerBoundaryAndDisable(t *testing.T) {
	const threshold = 512
	mk := func(n, t int) string {
		return headerOf(Message(store.Message{
			PublicID:  "cc03",
			FromAgent: "bosun",
			ToAgent:   "pilot",
			Body:      strings.Repeat("y", n),
			CreatedAt: "2026-06-07T09:02:00.000Z",
		}, t))
	}
	// Exactly at threshold → no marker (strict ">" semantics).
	if h := mk(threshold, threshold); hasMarker(h) {
		t.Errorf("body == threshold should have no marker: %q", h)
	}
	// One byte over → marker (sub-1k form: `513b`).
	if h := mk(threshold+1, threshold); !strings.Contains(h, "· 513b]") {
		t.Errorf("body == threshold+1 should carry `· 513b]`: %q", h)
	}
	// Negative threshold disables the marker even for a huge body.
	if h := mk(5000, -1); hasMarker(h) {
		t.Errorf("negative threshold should disable the marker: %q", h)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int]string{
		0:     "0b",
		512:   "512b",
		999:   "999b",
		1000:  "1.0k",
		2300:  "2.3k",
		2347:  "2.3k",
		15000: "15.0k",
	}
	for n, want := range cases {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestFormatClock_FallsBackOnBadInput(t *testing.T) {
	if got := formatClock("not a timestamp"); got != "??:??:??" {
		t.Errorf("formatClock(bad) = %q, want fallback", got)
	}
}

func TestFormatClock_AcceptsBothLayouts(t *testing.T) {
	// Expected output is local-tz; compute it from the input so the test
	// passes in any timezone (CI = UTC, alcatraz host = Europe/Berlin).
	for _, iso := range []string{
		"2026-05-29T11:04:12.000Z",
		"2026-05-29T11:04:12Z",
	} {
		want := localClockFromUTC(t, iso)
		if got := formatClock(iso); got != want {
			t.Errorf("formatClock(%q) = %q, want %q", iso, got, want)
		}
	}
}

func TestTitleCase(t *testing.T) {
	cases := map[string]string{
		"":       "",
		"a":      "A",
		"bosun":  "Bosun",
		"BOSUN":  "BOSUN",
		"alex42": "Alex42",
	}
	for in, want := range cases {
		if got := titleCase(in); got != want {
			t.Errorf("titleCase(%q) = %q, want %q", in, got, want)
		}
	}
}
