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
// (so the mailman serves its pane and reaches deliverOne) with the #420
// per-(command, adapter) control-command allowlist — codex implements only
// /compact, /rename, /clear, /help; /mcp … and /cost are unsupported and skip.
// Distinct from serve_paste_capability_test.go's codexLikeProfile, which is
// PasteCapable=false (the #323 force-defer case).
var codexPasteCapableProfile = Profile{
	BinaryName:   "tmux-tell-codex",
	DisplayLabel: "Codex",
	PasteCapable: true,
	SupportedControlCommands: map[string]bool{
		"/compact": true,
		"/rename":  true,
		"/clear":   true,
		"/help":    true,
	},
	Pane: tmuxio.CodexPaneProfile(),
}

// TestControlCommandToken pins the #420 token extractor: the leading
// whitespace-delimited field, whitespace-tolerant, leading-token only (so a
// non-leading occurrence or a longer first token keys distinctly).
func TestControlCommandToken(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{"/mcp disable tmux-tell", "/mcp"},
		{"/mcp", "/mcp"},
		{"  /mcp disable tmux-tell  ", "/mcp"}, // whitespace-tolerant
		{"/mcp\tdisable tmux-msg", "/mcp"},     // tab separator (Lookout #421 review)
		{"/mcp\ndisable tmux-msg", "/mcp"},     // newline separator
		{"/mcp\t\n  disable tmux-msg", "/mcp"}, // mixed whitespace run
		{"/compact", "/compact"},
		{"/cost", "/cost"},
		{"/help", "/help"},
		{"", ""},                          // blank → empty token
		{"   \t ", ""},                    // all-whitespace → empty token
		{"/mcpfoo", "/mcpfoo"},            // word boundary — a distinct token, not "/mcp"
		{"please /mcp disable", "please"}, // not leading — keys on the real first token
	}
	for _, tc := range cases {
		if got := controlCommandToken(tc.body); got != tc.want {
			t.Errorf("controlCommandToken(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// TestAdapterSupportsControl pins the #420 capability gate on BOTH adapters:
// Claude's nil set = supports-all; codex's explicit allowlist gates per token.
func TestAdapterSupportsControl(t *testing.T) {
	// Claude default (active is the package-default Claude profile, nil set).
	for _, body := range []string{"/mcp disable tmux-tell", "/cost", "/compact", "/anything-future"} {
		if !adapterSupportsControl(body) {
			t.Errorf("claude (nil set) should support every control command; %q reported unsupported", body)
		}
	}

	withActiveProfile(t, codexPasteCapableProfile)
	supported := []string{"/compact", "/rename", "/clear", "/help", "  /compact  ", "/rename to Bob"}
	for _, body := range supported {
		if !adapterSupportsControl(body) {
			t.Errorf("codex should support %q (leading token in its allowlist)", body)
		}
	}
	unsupported := []string{"/mcp disable tmux-tell", "/mcp enable tmux-tell", "/cost", "/compactfoo", ""}
	for _, body := range unsupported {
		if adapterSupportsControl(body) {
			t.Errorf("codex should NOT support %q (leading token absent from its allowlist)", body)
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

// TestDeliverOne_Codex_SkipsUnsupportedControl: control commands codex's CLI
// lacks (#420 covers /cost in addition to #419's /mcp …) return
// errControlUnsupported and are NOT typed into the pane. ≥2 unsupported per AC.
func TestDeliverOne_Codex_SkipsUnsupportedControl(t *testing.T) {
	withActiveProfile(t, codexPasteCapableProfile)
	for _, body := range []string{"/mcp disable tmux-tell", "/cost"} {
		lits := recordSendKeysLiteral(t)
		msg := &store.Message{Kind: store.KindControl, Body: body, PublicID: "abc1"}
		err := deliverOne(context.Background(), "%3", msg, 0, nil)
		if !errors.Is(err, errControlUnsupported) {
			t.Fatalf("codex %q control: err = %v, want errControlUnsupported", body, err)
		}
		if len(*lits) != 0 {
			t.Errorf("codex %q control must NOT be typed; got send-keys -l %q", body, *lits)
		}
	}
}

// TestDeliverOne_Codex_PastesSupportedControl: control commands codex's CLI DOES
// implement are typed normally and return nil. ≥2 supported per AC. /compact is
// load-bearing — codex chambers sleep via the bus `sleep` verb → /compact Text.
func TestDeliverOne_Codex_PastesSupportedControl(t *testing.T) {
	withActiveProfile(t, codexPasteCapableProfile)
	for _, body := range []string{"/compact", "/rename"} {
		lits := recordSendKeysLiteral(t)
		msg := &store.Message{Kind: store.KindControl, Body: body, PublicID: "abc2"}
		if err := deliverOne(context.Background(), "%3", msg, 0, nil); err != nil {
			t.Fatalf("codex %q control: err = %v, want nil (in codex allowlist)", body, err)
		}
		if len(*lits) != 1 || (*lits)[0] != body {
			t.Errorf("codex %q control should type the body; got send-keys -l %q", body, *lits)
		}
	}
}

// TestDeliverOne_Claude_PastesAllControl: the Claude adapter (nil set =
// supports-all, #420) types every control command — including the ones codex
// skips — and returns nil.
func TestDeliverOne_Claude_PastesAllControl(t *testing.T) {
	// active defaults to the Claude profile (SupportedControlCommands == nil).
	for _, body := range []string{"/mcp disable tmux-tell", "/cost"} {
		lits := recordSendKeysLiteral(t)
		msg := &store.Message{Kind: store.KindControl, Body: body, PublicID: "abc3"}
		if err := deliverOne(context.Background(), "%3", msg, 0, nil); err != nil {
			t.Fatalf("claude %q control: err = %v, want nil", body, err)
		}
		if len(*lits) != 1 || (*lits)[0] != body {
			t.Errorf("claude %q control should type the body; got send-keys -l %q", body, *lits)
		}
	}
}

// TestServe_Codex_UnsupportedControl_SkippedDeliveredWithWarn is the serve-loop
// integration: an unsupported control row for a codex agent is marked delivered
// (consumed, not pasted) and logs a structured control_command_unsupported WARN.
// Uses /cost — a #420 addition over #419's /mcp — so the integration path is
// pinned on the generalized gate, not just the original narrow case.
func TestServe_Codex_UnsupportedControl_SkippedDeliveredWithWarn(t *testing.T) {
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
		Body: "/cost", Kind: store.KindControl,
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
		t.Fatalf("delivered = %d, want 1 (skipped /cost marked delivered); log=%s", len(delivered), logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "control_command_unsupported") {
		t.Errorf("expected control_command_unsupported WARN; log=%s", logbuf.String())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lits {
		if strings.Contains(l, "/cost") {
			t.Errorf("codex /cost must NOT be typed into the pane; got send-keys -l %q", l)
		}
	}
}
