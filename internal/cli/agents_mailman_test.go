package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestMailmanIdleHuman pins the agents-listing MAILMAN-column humanizer (#348).
func TestMailmanIdleHuman(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-06-13T12:00:00Z")
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	cases := []struct{ name, last, want string }{
		{"never", "", "never"},
		{"seconds", ago(30 * time.Second), "30s ago"},
		{"minutes", ago(5 * time.Minute), "5m ago"},
		{"hours", ago(3 * time.Hour), "3h ago"},
		{"days", ago(50 * time.Hour), "2d ago"},
		{"unparseable echoes raw", "not-a-time", "not-a-time"},
		{"future clamps to 0s", now.Add(10 * time.Second).Format(time.RFC3339Nano), "0s ago"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mailmanIdleHuman(c.last, now); got != c.want {
				t.Errorf("mailmanIdleHuman(%q) = %q, want %q", c.last, got, c.want)
			}
		})
	}

	// The store stamps fractional seconds (strftime %f) — confirm those parse
	// (yield a real duration, not the raw echo).
	t.Run("fractional store stamp parses", func(t *testing.T) {
		got := mailmanIdleHuman("2026-06-13T11:54:30.500Z", now)
		if got == "2026-06-13T11:54:30.500Z" || !strings.HasSuffix(got, "ago") {
			t.Errorf("fractional stamp didn't parse: got %q", got)
		}
	})
}

// TestAgentsListing_MailmanLastDelivered confirms the agents listing derives
// mailman_last_delivered_at from delivery rows (#348): set for a delivered
// agent, omitted for an undelivered one, and the text view shows "never".
func TestAgentsListing_MailmanLastDelivered(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()

	// Deliver one message to bob; carol gets nothing.
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "x", Kind: store.KindMessage}); err != nil {
		t.Fatal(err)
	}
	m, err := s.ClaimNext(ctx, "bob")
	if err != nil || m == nil {
		t.Fatalf("claim: %v / %v", m, err)
	}
	if err := s.MarkDelivered(ctx, m.PublicID); err != nil {
		t.Fatal(err)
	}

	// JSON: bob has the field, carol omits it.
	var out bytes.Buffer
	if code := runAgentsWithStore(ctx, s, map[string]bool{}, false, "json", &out, &out); code != exitOK {
		t.Fatalf("agents json exit=%d: %s", code, out.String())
	}
	var rows []agentView
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	byName := map[string]agentView{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if byName["bob"].MailmanLastDelivered == "" {
		t.Error("bob (delivered) should carry mailman_last_delivered_at")
	}
	if byName["carol"].MailmanLastDelivered != "" {
		t.Errorf("carol (no delivery) should omit mailman_last_delivered_at, got %q", byName["carol"].MailmanLastDelivered)
	}

	// Text: MAILMAN column present; undelivered carol shows "never".
	var tout bytes.Buffer
	runAgentsWithStore(ctx, s, map[string]bool{}, false, "text", &tout, &tout)
	if !strings.Contains(tout.String(), "MAILMAN") {
		t.Errorf("text header missing MAILMAN column:\n%s", tout.String())
	}
	if !strings.Contains(tout.String(), "never") {
		t.Errorf("text should show 'never' for undelivered carol:\n%s", tout.String())
	}
}
