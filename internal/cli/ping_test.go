package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
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
		state string
		want  int
	}{
		{string(store.StateDelivered), exitOK},
		{string(store.StateFailed), exitUnavailable},
		{pingStateTimeout, exitTempFail},
	}
	for _, c := range cases {
		if got := pingExitCode(pingResult{State: c.state}); got != c.want {
			t.Errorf("pingExitCode(%s) = %d, want %d", c.state, got, c.want)
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
