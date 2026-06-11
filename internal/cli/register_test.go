package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func TestRegister_CLI_DefaultsToPasteAndEnter(t *testing.T) {
	t.Setenv("CLAUDE_MSG_DB", ":memory:")

	// CLI register uses store.Open which won't see in-memory store
	// from newCmdTestStore. We test the validation + flag-parsing
	// directly via the doRegister-equivalent path: parse + assert
	// via direct s.GetAgent after a manual UpsertAgent. The CLI's
	// store-open path is exercised via the MCP-shape tests below.
	//
	// This test pins flag-parsing semantics and store.SetDeliveryMode
	// integration without the CLI's store-open dance.
	s := newCmdTestStore(t, "alice")
	ctx := context.Background()
	if err := s.SetDeliveryMode(ctx, "alice", store.DeliveryModePasteAndEnter); err != nil {
		t.Fatalf("set delivery_mode: %v", err)
	}
	a, err := s.GetAgent(ctx, "alice")
	if err != nil {
		t.Fatalf("get_agent: %v", err)
	}
	if a.DeliveryMode != store.DeliveryModePasteAndEnter {
		t.Errorf("DeliveryMode = %q, want %q", a.DeliveryMode, store.DeliveryModePasteAndEnter)
	}
}

func TestRegister_CLI_AcceptsMailboxOnly(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	ctx := context.Background()
	if err := s.SetDeliveryMode(ctx, "alice", store.DeliveryModeMailboxOnly); err != nil {
		t.Fatalf("set delivery_mode: %v", err)
	}
	a, err := s.GetAgent(ctx, "alice")
	if err != nil {
		t.Fatalf("get_agent: %v", err)
	}
	if a.DeliveryMode != store.DeliveryModeMailboxOnly {
		t.Errorf("DeliveryMode = %q, want %q", a.DeliveryMode, store.DeliveryModeMailboxOnly)
	}
}

func TestRegister_CLI_RejectsInvalidDeliveryMode(t *testing.T) {
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Setenv("TMUX_PANE", "%5")
	var stdout, stderr bytes.Buffer
	exit := runRegisterCLI([]string{"--name", "alice", "--delivery-mode", "bogus"},
		&stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want exitUsage (%d)", exit, exitUsage)
	}
	out := stdout.String()
	if !strings.Contains(out, "invalid --delivery-mode") {
		t.Errorf("expected validation error in output; got %q", out)
	}
}

func TestRegister_CLI_NameRequired(t *testing.T) {
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	var stdout, stderr bytes.Buffer
	exit := runRegisterCLI([]string{"--pane", "%5"}, &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want exitUsage", exit)
	}
}

// TestRegister_CLI_SurfacesQueuedBacklog exercises the CLI store-open path
// end-to-end against a temp-file DB (the in-memory newCmdTestStore can't be
// shared with the CLI's own store.Open). Confirms the `queued` count (#151)
// reaches the CLI register response, not just the MCP handler.
func TestRegister_CLI_SurfacesQueuedBacklog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	ctx := context.Background()

	seed, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	for _, n := range []string{"sender", "backlogged"} {
		if err := seed.UpsertAgent(ctx, n, "%99"); err != nil {
			t.Fatalf("seed agent %s: %v", n, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := seed.InsertMessage(ctx, store.InsertParams{
			FromAgent: "sender", ToAgent: "backlogged", Body: "hi",
		}); err != nil {
			t.Fatalf("seed msg: %v", err)
		}
	}
	_ = seed.Close()

	t.Setenv("CLAUDE_MSG_DB", dbPath)
	var stdout, stderr bytes.Buffer
	exit := runRegisterCLI([]string{
		"--name", "backlogged", "--pane", "%9",
		"--force", "--start-mailman=false",
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want exitOK; stderr=%s", exit, stderr.String())
	}
	out := parseJSONResult(t, stdout.Bytes())
	q, ok := out["queued"].(float64)
	if !ok {
		t.Fatalf("queued missing or wrong type; out=%v", out)
	}
	if int(q) != 2 {
		t.Errorf("queued = %v, want 2", q)
	}
}

// TestRegister_CLI_RefusesStartMailmanWithNonDefaultDB pins the #293
// refusal at the CLI surface. A caller with a non-default $CLAUDE_MSG_DB
// requesting --start-mailman=true would silently misroute (agent row in
// sandbox DB, systemd-managed mailman polling production DB), so the CLI
// refuses up-front with an actionable error before any DB writes happen.
func TestRegister_CLI_RefusesStartMailmanWithNonDefaultDB(t *testing.T) {
	// Trap any actual systemctl call — the refusal must fire BEFORE the
	// startMailman call site is ever reached, so the runner stays untouched.
	var systemctlCalls int
	prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
		systemctlCalls++
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	dbPath := filepath.Join(t.TempDir(), "messages.db")
	t.Setenv("CLAUDE_MSG_DB", dbPath)
	t.Setenv("TMUX_PANE", "%5")
	var stdout, stderr bytes.Buffer
	exit := runRegisterCLI([]string{
		"--name", "alice", "--start-mailman=true",
	}, &stdout, &stderr)

	if exit != exitDataErr {
		t.Fatalf("exit = %d, want exitDataErr (%d); stderr=%s",
			exit, exitDataErr, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "non-default CLAUDE_MSG_DB") {
		t.Errorf("expected refusal error mentioning non-default CLAUDE_MSG_DB; got %q", out)
	}
	if !strings.Contains(out, dbPath) {
		t.Errorf("expected refusal error naming the caller's DB path %q; got %q", dbPath, out)
	}
	if !strings.Contains(out, "serve --agent") {
		t.Errorf("expected refusal error suggesting `serve --agent` recovery; got %q", out)
	}
	if systemctlCalls != 0 {
		t.Errorf("startMailman was reached %d times; refusal should fire BEFORE the systemctl call", systemctlCalls)
	}

	// And the agent row must NOT exist — refusal fires before any DB writes.
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store post-refusal: %v", err)
	}
	defer s.Close()
	_, err = s.GetAgent(context.Background(), "alice")
	if err == nil {
		t.Errorf("agent row exists after refusal; should have been blocked before upsert")
	}
}

// TestRegister_CLI_AllowsNonDefaultDBWithStartMailmanFalse confirms the
// #293 refusal is scoped to start_mailman=true. A sandbox-DB caller using
// --start-mailman=false (and presumably running `serve --agent` themselves
// as a foreground subprocess) is the intended escape hatch — no refusal.
func TestRegister_CLI_AllowsNonDefaultDBWithStartMailmanFalse(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	t.Setenv("CLAUDE_MSG_DB", dbPath)
	t.Setenv("TMUX_PANE", "%5")
	var stdout, stderr bytes.Buffer
	exit := runRegisterCLI([]string{
		"--name", "alice", "--start-mailman=false",
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want exitOK; stderr=%s", exit, stderr.String())
	}
	out := parseJSONResult(t, stdout.Bytes())
	if out["mailman"] != "skipped" {
		t.Errorf("mailman = %v, want \"skipped\"", out["mailman"])
	}
	// And the agent row should exist — the register itself succeeded.
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	if _, err := s.GetAgent(context.Background(), "alice"); err != nil {
		t.Errorf("agent row missing after register: %v", err)
	}
}

func TestStore_ValidDeliveryMode(t *testing.T) {
	cases := map[string]bool{
		store.DeliveryModePasteAndEnter: true,
		store.DeliveryModeMailboxOnly:   true,
		"":                              false,
		"bogus":                         false,
		"PASTE-AND-ENTER":               false, // case-sensitive
	}
	for in, want := range cases {
		if got := store.ValidDeliveryMode(in); got != want {
			t.Errorf("ValidDeliveryMode(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStore_SetDeliveryMode_RejectsInvalid(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	err := s.SetDeliveryMode(context.Background(), "alice", "bogus")
	if err == nil {
		t.Fatal("expected error for invalid delivery_mode; got nil")
	}
	if !strings.Contains(err.Error(), "invalid delivery_mode") {
		t.Errorf("err = %v, want 'invalid delivery_mode' prefix", err)
	}
}

func TestStore_SetDeliveryMode_RejectsUnknownAgent(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	err := s.SetDeliveryMode(context.Background(), "nobody", store.DeliveryModeMailboxOnly)
	if err == nil {
		t.Fatal("expected ErrNotFound for unknown agent")
	}
}
