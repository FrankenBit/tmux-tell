package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestPingHealthy exercises the substrate-health predicate directly —
// the load-bearing branch of the mailman's ping handling — without
// needing a running mailman. Pane liveness is faked via the LivePanes
// runner seam (#144).
func TestPingHealthy(t *testing.T) {
	ctx := context.Background()

	t.Run("empty pane id fails", func(t *testing.T) {
		reason, ok := pingHealthy(ctx, "")
		if ok {
			t.Fatal("ok=true, want false for empty pane id")
		}
		if !strings.Contains(reason, "no pane_id") {
			t.Errorf("reason = %q, want mention of no pane_id", reason)
		}
	})

	t.Run("live pane ok", func(t *testing.T) {
		prev := tmuxio.SetListPanesRunner(func(_ context.Context) ([]byte, error) {
			return []byte("%1\n%3\n"), nil
		})
		t.Cleanup(func() { tmuxio.SetListPanesRunner(prev) })
		reason, ok := pingHealthy(ctx, "%3")
		if !ok {
			t.Fatalf("ok=false (%s), want true for live pane", reason)
		}
	})

	t.Run("dead pane fails", func(t *testing.T) {
		prev := tmuxio.SetListPanesRunner(func(_ context.Context) ([]byte, error) {
			return []byte("%1\n"), nil
		})
		t.Cleanup(func() { tmuxio.SetListPanesRunner(prev) })
		reason, ok := pingHealthy(ctx, "%3")
		if ok {
			t.Fatal("ok=true, want false for dead pane")
		}
		if !strings.Contains(reason, "not live") {
			t.Errorf("reason = %q, want mention of not live", reason)
		}
	})

	t.Run("probe error fails", func(t *testing.T) {
		prev := tmuxio.SetListPanesRunner(func(_ context.Context) ([]byte, error) {
			return nil, &errString{"boom"}
		})
		t.Cleanup(func() { tmuxio.SetListPanesRunner(prev) })
		reason, ok := pingHealthy(ctx, "%3")
		if ok {
			t.Fatal("ok=true, want false on probe error")
		}
		if !strings.Contains(reason, "probe failed") {
			t.Errorf("reason = %q, want mention of probe failed", reason)
		}
	})
}

func TestInsertPing(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")

	t.Run("registered recipient inserts a queued ping row", func(t *testing.T) {
		id, err := insertPing(ctx, s, "alice", "bob")
		if err != nil {
			t.Fatalf("insertPing: %v", err)
		}
		if id == "" {
			t.Fatal("empty id")
		}
		m, err := s.GetMessage(ctx, id)
		if err != nil {
			t.Fatalf("GetMessage: %v", err)
		}
		if m.Kind != store.KindPing {
			t.Errorf("kind = %s, want %s", m.Kind, store.KindPing)
		}
		if m.State != store.StateQueued {
			t.Errorf("state = %s, want queued", m.State)
		}
	})

	t.Run("unknown recipient fails loud", func(t *testing.T) {
		_, err := insertPing(ctx, s, "alice", "ghost")
		if err == nil {
			t.Fatal("want error for unknown recipient")
		}
		if !strings.Contains(err.Error(), "unknown recipient") {
			t.Errorf("err = %v, want 'unknown recipient'", err)
		}
	})

	t.Run("empty from fails", func(t *testing.T) {
		if _, err := insertPing(ctx, s, "", "bob"); err == nil {
			t.Fatal("want error for empty from")
		}
	})

	t.Run("empty to fails", func(t *testing.T) {
		if _, err := insertPing(ctx, s, "alice", ""); err == nil {
			t.Fatal("want error for empty to")
		}
	})
}

func TestPollPingTerminal(t *testing.T) {
	ctx := context.Background()

	t.Run("delivered", func(t *testing.T) {
		s, _ := store.Open(":memory:")
		t.Cleanup(func() { _ = s.Close() })
		_ = s.UpsertAgent(ctx, "bob", "%3")
		id, _ := insertPing(ctx, s, "alice", "bob")
		_, _ = s.ClaimNext(ctx, "bob")
		_ = s.MarkDelivered(ctx, id)

		res, err := pollPingTerminal(ctx, s, id, "bob", time.Second, 5*time.Millisecond)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if !res.OK || res.State != string(store.StateDelivered) {
			t.Errorf("res = %+v, want delivered+ok", res)
		}
		if res.Agent != "bob" || res.ID != id {
			t.Errorf("res = %+v, want agent=bob id=%s", res, id)
		}
	})

	t.Run("failed surfaces the store error", func(t *testing.T) {
		s, _ := store.Open(":memory:")
		t.Cleanup(func() { _ = s.Close() })
		_ = s.UpsertAgent(ctx, "bob", "%3")
		id, _ := insertPing(ctx, s, "alice", "bob")
		_, _ = s.ClaimNext(ctx, "bob")
		_ = s.MarkFailed(ctx, id, "registered pane %3 is not live (agent unreachable)")

		res, err := pollPingTerminal(ctx, s, id, "bob", time.Second, 5*time.Millisecond)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if res.OK {
			t.Error("ok=true, want false")
		}
		if res.State != string(store.StateFailed) {
			t.Errorf("state = %s, want failed", res.State)
		}
		if !strings.Contains(res.Error, "not live") {
			t.Errorf("error = %q, want mention of not live", res.Error)
		}
	})

	t.Run("timeout when no mailman processes the row", func(t *testing.T) {
		s, _ := store.Open(":memory:")
		t.Cleanup(func() { _ = s.Close() })
		_ = s.UpsertAgent(ctx, "bob", "%3")
		id, _ := insertPing(ctx, s, "alice", "bob")

		res, err := pollPingTerminal(ctx, s, id, "bob", 20*time.Millisecond, 5*time.Millisecond)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if res.OK {
			t.Error("ok=true, want false on timeout")
		}
		if res.State != pingStateTimeout {
			t.Errorf("state = %s, want %s", res.State, pingStateTimeout)
		}
	})
}

func TestRunPingWithStore_BadFormatIsUsageError(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	_ = s.UpsertAgent(context.Background(), "bob", "%3")

	var out, errb bytes.Buffer
	exit := runPingWithStore(context.Background(), s,
		pingCLIParams{From: "alice", To: "bob", Timeout: 20 * time.Millisecond, Format: "yaml"},
		&out, &errb)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d", exit, exitUsage)
	}
}

func TestRunPingWithStore_UnknownRecipient(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var out, errb bytes.Buffer
	exit := runPingWithStore(context.Background(), s,
		pingCLIParams{From: "alice", To: "ghost", Timeout: 20 * time.Millisecond, Format: "json"},
		&out, &errb)
	if exit != exitUnavailable {
		t.Errorf("exit = %d, want %d", exit, exitUnavailable)
	}
	var resp map[string]any
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("stdout not JSON: %v (out=%q)", err, out.String())
	}
	if resp["ok"] != false {
		t.Errorf("ok = %v, want false", resp["ok"])
	}
}

func TestPingExitCode(t *testing.T) {
	cases := []struct {
		class pingClass
		want  int
	}{
		{classReachable, exitOK},
		{classPending, exitTempFail},
		{classUnreachable, exitUnavailable},
	}
	for _, c := range cases {
		if got := pingExitCode(pingResult{Class: c.class}); got != c.want {
			t.Errorf("pingExitCode(class=%s) = %d, want %d", c.class, got, c.want)
		}
	}
}

// TestReachabilityClass proves the #366 reason→class map: a confirmed delivery
// is reachable, the two healthy-but-unconfirmed reasons are pending (notably
// blocked_delivery UNCONDITIONALLY — a ping never reaches the observe-gate), and
// the three substrate-broken reasons are unreachable. All three classes are
// produced, mirroring the AC#4-style coverage proof on classifyPingReason.
func TestReachabilityClass(t *testing.T) {
	cases := []struct {
		name string
		res  pingResult
		want pingClass
	}{
		{"delivered → reachable", pingResult{OK: true, State: string(store.StateDelivered)}, classReachable},
		{"backlog_draining → pending", pingResult{Reason: reasonBacklogDraining}, classPending},
		{"blocked_delivery → pending (unconditional, #366)", pingResult{Reason: reasonBlockedDelivery}, classPending},
		{"mailman_down → unreachable", pingResult{Reason: reasonMailmanDown}, classUnreachable},
		{"stuck → unreachable", pingResult{Reason: reasonStuck}, classUnreachable},
		{"pane_dead → unreachable", pingResult{Reason: reasonPaneDead}, classUnreachable},
	}
	seen := map[pingClass]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reachabilityClass(tc.res); got != tc.want {
				t.Errorf("reachabilityClass(%+v) = %q, want %q", tc.res, got, tc.want)
			}
		})
		seen[tc.want] = true
	}
	for _, c := range []pingClass{classReachable, classPending, classUnreachable} {
		if !seen[c] {
			t.Errorf("no case produced class %q", c)
		}
	}
}

// TestPingExitCode_ReasonChain documents the #366 exit-code contract end-to-end
// (reason → class → sysexits code), including the deliberate shift of
// mailman_down + stuck from exitTempFail (pre-#366, keyed on state=timeout) to
// exitUnavailable (now keyed on class=unreachable) — a down or parked mailman
// won't self-heal on retry, so tempfail over-promised recoverability.
func TestPingExitCode_ReasonChain(t *testing.T) {
	cases := []struct {
		reason pingReason
		want   int
		note   string
	}{
		{reasonBacklogDraining, exitTempFail, "pending — unchanged"},
		{reasonBlockedDelivery, exitTempFail, "pending — unchanged"},
		{reasonMailmanDown, exitUnavailable, "#366 shift: was exitTempFail"},
		{reasonStuck, exitUnavailable, "#366 shift: was exitTempFail"},
		{reasonPaneDead, exitUnavailable, "unreachable — unchanged"},
	}
	for _, c := range cases {
		res := pingResult{State: pingStateTimeout, Reason: c.reason}
		res.Class = reachabilityClass(res)
		if got := pingExitCode(res); got != c.want {
			t.Errorf("reason %s → class %s → exit %d, want %d (%s)", c.reason, res.Class, got, c.want, c.note)
		}
	}
}

// TestPingSandboxMsg covers #809 via the pure helper — root-independent because
// it injects fabricated errors rather than relying on filesystem chmod tricks
// that CAP_DAC_OVERRIDE bypasses under CI's uid=0 runner.
func TestPingSandboxMsg(t *testing.T) {
	t.Run("readonly database error produces sandbox diagnostic", func(t *testing.T) {
		// Fabricate the exact error shape SQLite emits under sandbox FS scope
		// (SQLITE_READONLY family; PRAGMA journal_mode = WAL denied).
		err := errors.New("store: PRAGMA journal_mode = WAL: attempt to write a readonly database (1544)")
		msg := pingSandboxMsg(err)
		for _, want := range []string{"sandbox", "tmux-tell.ping", "write access"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message missing %q:\n%s", want, msg)
			}
		}
		// Must NOT include the plain prefix (that is the non-sandbox path).
		if strings.Contains(msg, "open store:") {
			t.Errorf("sandbox message double-wrapped with 'open store:' prefix:\n%s", msg)
		}
	})

	t.Run("non-readonly error preserves original text", func(t *testing.T) {
		// EACCES or any other non-readonly open failure must pass through unchanged.
		err := errors.New("open /path/to/db: permission denied")
		msg := pingSandboxMsg(err)
		if strings.Contains(msg, "sandbox") {
			t.Errorf("sandbox diagnostic emitted for non-readonly error:\n%s", msg)
		}
		if !strings.Contains(msg, "open store:") {
			t.Errorf("original prefix missing:\n%s", msg)
		}
		if !strings.Contains(msg, "permission denied") {
			t.Errorf("original error text missing:\n%s", msg)
		}
	})

	t.Run("migration readonly error also triggers diagnostic", func(t *testing.T) {
		// A DB that opens but fails a migration write produces error code 8
		// (vs 1544 for the WAL pragma); the helper must catch both.
		err := errors.New(`store: migrate "ALTER TABLE ...": attempt to write a readonly database (8)`)
		msg := pingSandboxMsg(err)
		if !strings.Contains(msg, "sandbox") {
			t.Errorf("sandbox diagnostic missing for migration readonly error:\n%s", msg)
		}
	})
}

// TestPingCLI_SandboxIntegration is a filesystem-level integration check for
// #809. Skipped under uid=0 because CAP_DAC_OVERRIDE writes through 0444 —
// the filesystem signal is absent for root, so the test cannot exercise the
// path it exists to verify. The pure-helper tests above cover the detection
// logic unconditionally.
func TestPingCLI_SandboxIntegration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("uid=0: CAP_DAC_OVERRIDE bypasses 0444 — filesystem signal absent")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "messages.db")
	seed, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = seed.Close()
	if err := os.Chmod(dbPath, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o644) })

	var stdout, stderr bytes.Buffer
	exit := runPingCLI([]string{"--db", dbPath, "anyagent"}, &stdout, &stderr)
	if exit == 0 {
		t.Fatal("exit=0, want non-zero for readonly store")
	}
	out := stdout.String() + stderr.String()
	for _, want := range []string{"sandbox", "tmux-tell.ping", "write access"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderPingResult(t *testing.T) {
	t.Run("json round-trips the struct", func(t *testing.T) {
		var out bytes.Buffer
		renderPingResult(&out, pingResult{
			OK: true, Agent: "bob", ID: "abcd", State: "delivered", ElapsedMs: 12,
		}, "json")
		var resp pingResult
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("json: %v", err)
		}
		if !resp.OK || resp.Agent != "bob" || resp.State != "delivered" {
			t.Errorf("resp = %+v", resp)
		}
	})

	t.Run("text surfaces the unreachable status + error", func(t *testing.T) {
		var out bytes.Buffer
		renderPingResult(&out, pingResult{
			OK: false, Agent: "bob", ID: "abcd", State: "failed", ElapsedMs: 5, Error: "pane gone",
		}, "text")
		s := out.String()
		for _, want := range []string{"bob", "failed", "UNREACHABLE", "pane gone"} {
			if !strings.Contains(s, want) {
				t.Errorf("text %q missing %q", s, want)
			}
		}
	})
}
