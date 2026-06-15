package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

func TestWhoami_RegisteredLive(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bosun", "%1")
	live := map[string]bool{"%1": true}

	var stdout bytes.Buffer
	exit := runWhoamiWithStore(ctx, s, live, "bosun", "explicit", "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true || got["name"] != "bosun" || got["pane_status"] != "live" {
		t.Errorf("got %v", got)
	}
}

func TestWhoami_NotRegistered(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runWhoamiWithStore(context.Background(), s, map[string]bool{},
		"ghost", "explicit", "json", &stdout, &stderr)
	if exit != exitUnavailable {
		t.Errorf("exit = %d, want %d", exit, exitUnavailable)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != false || got["name"] != "ghost" {
		t.Errorf("got %v", got)
	}
	if got["registered"] != false {
		t.Errorf("registered should be false: %v", got)
	}
}

func TestWhoami_TextFormat(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bosun", "%1")
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "bosun", ToAgent: "bosun", Body: "self test",
	})

	var stdout bytes.Buffer
	exit := runWhoamiWithStore(context.Background(), s,
		map[string]bool{"%1": true}, "bosun", "explicit", "text", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	out := stdout.String()
	for _, want := range []string{"NAME\tbosun", "PANE\t%1 (live)", "PAUSED\tno", "INBOX\t1 queued", "MODE\tpaste-and-enter"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestWhoami_ReportsDeliveryMode pins the MODE line (text) + delivery_mode field
// (JSON) for BOTH delivery modes — install.sh's codex bootstrap reads this to
// route paste-served vs hook-context agents without a JSON parser (#438).
func TestWhoami_ReportsDeliveryMode(t *testing.T) {
	for _, mode := range []string{store.DeliveryModePasteAndEnter, store.DeliveryModeHookContext} {
		t.Run(mode, func(t *testing.T) {
			s, _ := store.Open(":memory:")
			t.Cleanup(func() { _ = s.Close() })
			ctx := context.Background()
			_ = s.UpsertAgent(ctx, "lookout", "%8")
			if err := s.SetDeliveryMode(ctx, "lookout", mode); err != nil {
				t.Fatalf("set mode: %v", err)
			}

			var textOut, jsonOut bytes.Buffer
			_ = runWhoamiWithStore(ctx, s, map[string]bool{"%8": true}, "lookout", "explicit", "text", &textOut, &bytes.Buffer{})
			if want := "MODE\t" + mode; !strings.Contains(textOut.String(), want) {
				t.Errorf("text missing %q in:\n%s", want, textOut.String())
			}

			_ = runWhoamiWithStore(ctx, s, map[string]bool{"%8": true}, "lookout", "explicit", "json", &jsonOut, &bytes.Buffer{})
			var got map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(jsonOut.Bytes()), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got["delivery_mode"] != mode {
				t.Errorf("json delivery_mode = %v, want %s", got["delivery_mode"], mode)
			}
		})
	}
}

func TestWhoami_NoIdentity(t *testing.T) {
	// This exercises the CLI wrapper, since identity resolution lives there.
	t.Setenv("TMUX_AGENT_NAME", "")
	var stdout, stderr bytes.Buffer
	exit := runWhoamiCLI([]string{"--db", ":memory:"}, &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d", exit, exitUsage)
	}
}

// validate that JSON output parses for status helpers too
func TestWhoami_JSONRoundTrip(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bosun", "")
	var stdout bytes.Buffer
	_ = runWhoamiWithStore(ctx, s, nil, "bosun", "explicit", "json", &stdout, &bytes.Buffer{})
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["pane_status"] != "no-pane" {
		t.Errorf("pane_status = %v, want no-pane", got["pane_status"])
	}
}
