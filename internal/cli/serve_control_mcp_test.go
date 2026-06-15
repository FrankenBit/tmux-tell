package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// codexPasteCapableProfile mirrors the post-#360 codex Profile: paste-capable
// (so the mailman serves its pane and reaches deliverOne) but WITHOUT the `/mcp`
// slash command — the #419 case. Distinct from serve_paste_capability_test.go's
// codexLikeProfile, which is PasteCapable=false (the #323 force-defer case).
var codexPasteCapableProfile = Profile{
	BinaryName:              "tmux-tell-codex",
	DisplayLabel:            "Codex",
	PasteCapable:            true,
	SupportsMCPSlashCommand: false,
	Pane:                    tmuxio.CodexPaneProfile(),
}

// TestIsMCPControlCommand pins the #419 detector: every `/mcp …` variant
// (disable/enable/restart + bare) is caught, whitespace-tolerant, leading-token
// only (so a non-/mcp body that merely contains the substring is not caught).
func TestIsMCPControlCommand(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"/mcp disable tmux-msg", true},
		{"/mcp enable tmux-msg", true},
		{"/mcp restart tmux-msg", true},
		{"/mcp", true},
		{"  /mcp disable tmux-msg  ", true},  // whitespace-tolerant
		{"/mcp\tdisable tmux-msg", true},     // tab separator (Lookout #421 review)
		{"/mcp\ndisable tmux-msg", true},     // newline separator
		{"/mcp\t\n  disable tmux-msg", true}, // mixed whitespace run
		{"/compact", false},
		{"/cost", false},
		{"/help", false},
		{"", false},
		{"/mcpfoo", false},             // word boundary — not `/mcp`
		{"please /mcp disable", false}, // not leading
	}
	for _, tc := range cases {
		if got := isMCPControlCommand(tc.body); got != tc.want {
			t.Errorf("isMCPControlCommand(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// recordSendKeysLiteral installs a fake tmux runner that records every
// `send-keys -t <pane> -l <body>` literal body, and returns the recorder.
func recordSendKeysLiteral(t *testing.T) *[]string {
	t.Helper()
	var lits []string
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "send-keys" {
			for i, a := range args {
				if a == "-l" && i+1 < len(args) {
					lits = append(lits, args[i+1])
				}
			}
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
	return &lits
}

// TestDeliverOne_Codex_SkipsMCPControl: a `/mcp …` control message to the codex
// adapter returns errControlUnsupported and is NOT typed into the pane.
func TestDeliverOne_Codex_SkipsMCPControl(t *testing.T) {
	withActiveProfile(t, codexPasteCapableProfile)
	lits := recordSendKeysLiteral(t)

	msg := &store.Message{Kind: store.KindControl, Body: "/mcp disable tmux-msg", PublicID: "abc1"}
	err := deliverOne(context.Background(), "%3", msg, 0, nil)
	if !errors.Is(err, errControlUnsupported) {
		t.Fatalf("codex /mcp control: err = %v, want errControlUnsupported", err)
	}
	if len(*lits) != 0 {
		t.Errorf("codex /mcp control must NOT be typed; got send-keys -l %q", *lits)
	}
}

// TestDeliverOne_Claude_PastesMCPControl: the same `/mcp …` control message to
// the Claude adapter (which has /mcp) is typed normally and returns nil.
func TestDeliverOne_Claude_PastesMCPControl(t *testing.T) {
	// active defaults to the Claude profile (SupportsMCPSlashCommand=true).
	lits := recordSendKeysLiteral(t)

	msg := &store.Message{Kind: store.KindControl, Body: "/mcp disable tmux-msg", PublicID: "abc2"}
	if err := deliverOne(context.Background(), "%3", msg, 0, nil); err != nil {
		t.Fatalf("claude /mcp control: err = %v, want nil", err)
	}
	if len(*lits) != 1 || (*lits)[0] != "/mcp disable tmux-msg" {
		t.Errorf("claude /mcp control should type the body; got send-keys -l %q", *lits)
	}
}

// TestDeliverOne_Codex_PastesNonMCPControl: Option A is `/mcp`-only — a non-/mcp
// control command (`/compact`) on codex is still typed normally. (When #420's
// broader compat map lands, this test changes — surfacing the scope expansion.)
func TestDeliverOne_Codex_PastesNonMCPControl(t *testing.T) {
	withActiveProfile(t, codexPasteCapableProfile)
	lits := recordSendKeysLiteral(t)

	msg := &store.Message{Kind: store.KindControl, Body: "/compact", PublicID: "abc3"}
	if err := deliverOne(context.Background(), "%3", msg, 0, nil); err != nil {
		t.Fatalf("codex /compact control: err = %v, want nil (Option A is /mcp-only)", err)
	}
	if len(*lits) != 1 || (*lits)[0] != "/compact" {
		t.Errorf("codex /compact control should type the body; got send-keys -l %q", *lits)
	}
}

// TestServe_Codex_MCPControl_SkippedDeliveredWithWarn is the serve-loop
// integration: a `/mcp …` control row for a codex agent is marked delivered
// (consumed, not pasted) and logs a structured control_command_unsupported WARN.
func TestServe_Codex_MCPControl_SkippedDeliveredWithWarn(t *testing.T) {
	withActiveProfile(t, codexPasteCapableProfile)
	var (
		mu   sync.Mutex
		lits []string
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(args) > 0 && args[0] == "send-keys" {
			for i, a := range args {
				if a == "-l" && i+1 < len(args) {
					lits = append(lits, args[i+1])
				}
			}
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "lookout", "%3")
	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "lookout",
		Body: "/mcp disable tmux-msg", Kind: store.KindControl,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	stop, wait, logbuf := runServeInBackground(t, s, fastOpts("lookout"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "lookout", State: store.StateDelivered, Limit: 10,
		})
		if len(all) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	delivered, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "lookout", State: store.StateDelivered, Limit: 10,
	})
	if len(delivered) != 1 {
		t.Fatalf("delivered = %d, want 1 (skipped /mcp marked delivered); log=%s", len(delivered), logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "control_command_unsupported") {
		t.Errorf("expected control_command_unsupported WARN; log=%s", logbuf.String())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lits {
		if strings.Contains(l, "/mcp") {
			t.Errorf("codex /mcp must NOT be typed into the pane; got send-keys -l %q", l)
		}
	}
}
