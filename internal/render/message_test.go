package render

import (
	"database/sql"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

func TestMessage_Regular(t *testing.T) {
	got := Message(store.Message{
		PublicID:  "7f3a",
		FromAgent: "bosun",
		ToAgent:   "surveyor",
		Body:      "please check CI on PR 1234",
		CreatedAt: "2026-05-29T11:04:12.000Z",
	})
	wantSubstrings := []string{
		"Message from Bosun",
		"11:04:12",
		"id 7f3a",
		"please check CI on PR 1234",
		"────",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
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
		"Reply from Surveyor → Bosun",
		"re: 7f3a",
		"id 9c1d",
		"looking now, ETA 3 min",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	// Non-reply substring should be absent.
	if strings.Contains(got, "Message from") {
		t.Errorf("reply rendering should not say 'Message from': %s", got)
	}
}

func TestFormatClock_FallsBackOnBadInput(t *testing.T) {
	if got := formatClock("not a timestamp"); got != "??:??:??" {
		t.Errorf("formatClock(bad) = %q, want fallback", got)
	}
}

func TestFormatClock_AcceptsBothLayouts(t *testing.T) {
	if got := formatClock("2026-05-29T11:04:12.000Z"); got != "11:04:12" {
		t.Errorf("subsecond format: %q", got)
	}
	if got := formatClock("2026-05-29T11:04:12Z"); got != "11:04:12" {
		t.Errorf("plain format: %q", got)
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
