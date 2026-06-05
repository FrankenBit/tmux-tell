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
	})
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
	// Reply-format indicators should be absent on a regular message.
	if strings.Contains(got, " → ") || strings.Contains(got, " re ") {
		t.Errorf("regular rendering should not contain reply markers: %s", got)
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
	})
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
