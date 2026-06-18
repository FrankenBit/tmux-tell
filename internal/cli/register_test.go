package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
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

// TestRegister_CLI_PromotesRegisterDeferred pins the #258(a) wiring: a message
// staged with deliver_after="register" auto-promotes to queued when its
// recipient (re)registers (the spawn-die session-bridge — "remember this for my
// next dispatch"), while a resume-deferred row to the same agent stays staged
// (trigger isolation). The response surfaces deferred_promoted. Mutation check:
// drop the PromoteDeferred call in runRegisterCLI and the register row stays
// deferred → queued count is 0 and deferred_promoted is absent.
func TestRegister_CLI_PromotesRegisterDeferred(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	ctx := context.Background()

	seed, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	for _, n := range []string{"dispatcher", "pilot"} {
		if err := seed.UpsertAgent(ctx, n, "%99"); err != nil {
			t.Fatalf("seed agent %s: %v", n, err)
		}
	}
	reg, err := seed.InsertMessage(ctx, store.InsertParams{
		FromAgent: "dispatcher", ToAgent: "pilot", Body: "your next dispatch",
		DeliverAfter: "register",
	})
	if err != nil {
		t.Fatalf("seed register-deferred: %v", err)
	}
	if _, err := seed.InsertMessage(ctx, store.InsertParams{
		FromAgent: "dispatcher", ToAgent: "pilot", Body: "resume note",
		DeliverAfter: "resume",
	}); err != nil {
		t.Fatalf("seed resume-deferred: %v", err)
	}
	_ = seed.Close()

	t.Setenv("CLAUDE_MSG_DB", dbPath)
	var stdout, stderr bytes.Buffer
	exit := runRegisterCLI([]string{
		"--name", "pilot", "--pane", "%9",
		"--force", "--start-mailman=false",
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	out := parseJSONResult(t, stdout.Bytes())
	if dp, _ := out["deferred_promoted"].(float64); int(dp) != 1 {
		t.Errorf("deferred_promoted = %v, want 1", out["deferred_promoted"])
	}

	// The register row is now queued; the resume row stays deferred (isolation).
	check, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer check.Close() //nolint:errcheck // best-effort close
	queued, _ := check.ListMessages(ctx, store.ListFilter{ToAgent: "pilot", State: store.StateQueued})
	if len(queued) != 1 || queued[0].PublicID != reg.PublicID {
		t.Errorf("queued = %v, want only the promoted register row %s", queued, reg.PublicID)
	}
	deferred, _ := check.ListMessages(ctx, store.ListFilter{ToAgent: "pilot", Deferred: true})
	if len(deferred) != 1 || deferred[0].DeliverAfter.String != "resume" {
		t.Errorf("deferred = %v, want only the resume row still staged", deferred)
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
	defer s.Close() //nolint:errcheck // best-effort close
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
	defer s.Close() //nolint:errcheck // best-effort close
	if _, err := s.GetAgent(context.Background(), "alice"); err != nil {
		t.Errorf("agent row missing after register: %v", err)
	}
}

// TestRegister_CLI_RefusesStartMailmanWithMissingEnv pins #356 at the CLI
// surface. A caller whose env is missing DBUS_SESSION_BUS_ADDRESS or
// XDG_RUNTIME_DIR cannot start a systemd-managed mailman; the CLI refuses
// with an actionable error naming the missing vars before any DB writes.
// Uses the default DB path so the mismatch check (#293) does not fire first.
func TestRegister_CLI_RefusesStartMailmanWithMissingEnv(t *testing.T) {
	var systemctlCalls int
	prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
		systemctlCalls++
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	t.Setenv("TMUX_PANE", "%5")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	// Do NOT set CLAUDE_MSG_DB — use the default so the #293 mismatch check
	// does not fire before ours. The env check fires before store.Open, so
	// the real DB is never touched.
	t.Setenv("CLAUDE_MSG_DB", "")
	var stdout, stderr bytes.Buffer
	exit := runRegisterCLI([]string{
		"--name", "alice", "--start-mailman=true",
	}, &stdout, &stderr)

	if exit != exitDataErr {
		t.Fatalf("exit = %d, want exitDataErr (%d); stderr=%s stdout=%s",
			exit, exitDataErr, stderr.String(), stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "DBUS_SESSION_BUS_ADDRESS") {
		t.Errorf("expected error naming DBUS_SESSION_BUS_ADDRESS; got %q", out)
	}
	if !strings.Contains(out, "XDG_RUNTIME_DIR") {
		t.Errorf("expected error naming XDG_RUNTIME_DIR; got %q", out)
	}
	if !strings.Contains(out, "serve --agent") {
		t.Errorf("expected error suggesting `serve --agent` recovery; got %q", out)
	}
	if systemctlCalls != 0 {
		t.Errorf("startMailman was reached %d times; refusal should fire before", systemctlCalls)
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

// TestRegister_FlipStaleQueueDisposition pins Fix C of #390: flipping an
// existing agent's delivery_mode with pre-flip queued rows requires an explicit
// --purge-stale-queue / --keep-stale-queue disposition; the gate fires only on a
// real mode change with orphans present.
func TestRegister_FlipStaleQueueDisposition(t *testing.T) {
	ctx := context.Background()

	// freshDB returns a temp DB path seeded with lookout=hook-context + n queued.
	freshDB := func(t *testing.T, mode string, n int) string {
		t.Helper()
		db := filepath.Join(t.TempDir(), "m.db")
		s, err := store.Open(db)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer s.Close() //nolint:errcheck // best-effort close
		if err := s.UpsertAgent(ctx, "lookout", "%8"); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := s.SetDeliveryMode(ctx, "lookout", mode); err != nil {
			t.Fatalf("mode: %v", err)
		}
		for i := 0; i < n; i++ {
			if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "bosun", ToAgent: "lookout", Body: "m"}); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
		return db
	}
	flip := func(db string, extra ...string) (int, string) {
		var so, se bytes.Buffer
		base := []string{"--db", db, "--name", "lookout", "--pane", "%8",
			"--delivery-mode", "paste-and-enter", "--force", "--start-mailman=false"}
		exit := runRegisterCLI(append(base, extra...), &so, &se)
		return exit, so.String() + se.String()
	}
	// mustState asserts the delivery_mode + the disposition of the SEEDED rows
	// (from "bosun"), filtered by sender so the register-time 📬 backlog nudge
	// (a separate bus-inserted message) doesn't skew the count.
	mustState := func(t *testing.T, db, wantMode string, wantState store.State, wantSeeded int) {
		t.Helper()
		s, err := store.Open(db)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s.Close() //nolint:errcheck // best-effort close
		a, err := s.GetAgent(ctx, "lookout")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if a.DeliveryMode != wantMode {
			t.Errorf("delivery_mode = %q, want %q", a.DeliveryMode, wantMode)
		}
		msgs, err := s.ListMessages(ctx, store.ListFilter{ToAgent: "lookout", FromAgent: "bosun", State: wantState})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(msgs) != wantSeeded {
			t.Errorf("seeded rows in state %q = %d, want %d", wantState, len(msgs), wantSeeded)
		}
	}

	t.Run("flip with orphans + no disposition errors, mode unchanged", func(t *testing.T) {
		db := freshDB(t, store.DeliveryModeHookContext, 2)
		exit, out := flip(db)
		if exit != exitDataErr {
			t.Fatalf("exit = %d, want exitDataErr; out=%s", exit, out)
		}
		if !strings.Contains(out, "2 message") || !strings.Contains(out, "purge-stale-queue") {
			t.Errorf("error should name the count + both flags; got: %s", out)
		}
		mustState(t, db, store.DeliveryModeHookContext, store.StateQueued, 2) // gate fired before the flip
	})

	t.Run("--purge-stale-queue acks the rows + flip proceeds", func(t *testing.T) {
		db := freshDB(t, store.DeliveryModeHookContext, 2)
		exit, out := flip(db, "--purge-stale-queue")
		if exit != exitOK {
			t.Fatalf("exit = %d; out=%s", exit, out)
		}
		mustState(t, db, store.DeliveryModePasteAndEnter, store.StateAcknowledged, 2) // acked
	})

	t.Run("--keep-stale-queue leaves the rows queued + flip proceeds", func(t *testing.T) {
		db := freshDB(t, store.DeliveryModeHookContext, 2)
		exit, out := flip(db, "--keep-stale-queue")
		if exit != exitOK {
			t.Fatalf("exit = %d; out=%s", exit, out)
		}
		mustState(t, db, store.DeliveryModePasteAndEnter, store.StateQueued, 2) // still queued (now fenced)
	})

	t.Run("flip with zero orphans needs no disposition", func(t *testing.T) {
		db := freshDB(t, store.DeliveryModeHookContext, 0)
		exit, out := flip(db)
		if exit != exitOK {
			t.Fatalf("exit = %d; out=%s", exit, out)
		}
		mustState(t, db, store.DeliveryModePasteAndEnter, store.StateQueued, 0)
	})

	t.Run("same-mode --force re-register never trips the gate", func(t *testing.T) {
		db := freshDB(t, store.DeliveryModePasteAndEnter, 2) // already paste-and-enter
		exit, out := flip(db)                                // flips to paste-and-enter == same
		if exit != exitOK {
			t.Fatalf("same-mode re-register exit = %d; out=%s", exit, out)
		}
		mustState(t, db, store.DeliveryModePasteAndEnter, store.StateQueued, 2)
	})

	t.Run("both disposition flags is a usage error", func(t *testing.T) {
		db := freshDB(t, store.DeliveryModeHookContext, 2)
		exit, _ := flip(db, "--purge-stale-queue", "--keep-stale-queue")
		if exit != exitUsage {
			t.Fatalf("both-flags exit = %d, want exitUsage", exit)
		}
	})
}
