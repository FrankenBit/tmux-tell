package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestStartMailman_Success(t *testing.T) {
	var calls [][]string
	prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{}, args...))
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	if err := startMailman(context.Background(), "newpane"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	want := []string{"enable", "--now", "tmux-msg-claude-mailman@newpane.service"}
	for i, a := range want {
		if calls[0][i] != a {
			t.Errorf("call[0][%d] = %q, want %q", i, calls[0][i], a)
		}
	}
}

func TestStartMailman_PropagatesError(t *testing.T) {
	prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
		return []byte("Unit cannot be created"), errors.New("exit 1")
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	err := startMailman(context.Background(), "broken")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "Unit cannot be created") {
		t.Errorf("err didn't include systemd output: %v", err)
	}
}

// TestStartMailmanWouldMismatchSystemd_DefaultDBClean confirms the #293
// detection helper treats the production default path as "no mismatch" —
// the common-case caller (CLAUDE_MSG_DB unset, --db unset, resolved to
// defaultDBLocation) must not be refused.
func TestStartMailmanWouldMismatchSystemd_DefaultDBClean(t *testing.T) {
	mismatched, callerDB := startMailmanWouldMismatchSystemd(defaultDBLocation)
	if mismatched {
		t.Errorf("mismatched=true for the default DB path; should be false")
	}
	if callerDB != defaultDBLocation {
		t.Errorf("callerDB=%q, want defaultDBLocation %q", callerDB, defaultDBLocation)
	}
}

// TestStartMailmanWouldMismatchSystemd_SandboxDBFires confirms the #293
// detection helper fires for any non-default path. Substrate-honest about
// what the systemd-managed mailman would actually see (the unit file's
// production DB) vs the caller's resolved path.
func TestStartMailmanWouldMismatchSystemd_SandboxDBFires(t *testing.T) {
	cases := []string{
		"/tmp/sandbox.db",
		"/tmp/observe-gate-demo.db",
		"/var/lib/tmux-msg/messages2.db", // sibling-not-default
		":memory:",                       // test DB path
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			mismatched, callerDB := startMailmanWouldMismatchSystemd(p)
			if !mismatched {
				t.Errorf("mismatched=false for non-default DB %q; should fire", p)
			}
			if callerDB != p {
				t.Errorf("callerDB=%q, want %q", callerDB, p)
			}
		})
	}
}

// TestStartMailmanMismatchError_Actionable pins the structure of the
// caller-facing error: both DB paths named (caller's vs unit-file default),
// plus the foreground-`serve` recovery suggestion that does inherit the
// caller's env.
func TestStartMailmanMismatchError_Actionable(t *testing.T) {
	msg := startMailmanMismatchError("alice", "/tmp/sandbox.db")
	for _, frag := range []string{
		"/tmp/sandbox.db", // caller's DB
		defaultDBLocation, // unit file's DB
		"--start-mailman=false",
		"serve --agent alice",
	} {
		if !strings.Contains(msg, frag) {
			t.Errorf("error missing %q\nmsg=%s", frag, msg)
		}
	}
}

func TestStopMailman_IdempotentOnNotLoaded(t *testing.T) {
	cases := []string{
		"Failed to disable unit: Unit file tmux-msg-claude-mailman@.service does not exist.",
		"Unit tmux-msg-claude-mailman@ghost.service not loaded.",
		"No such file or directory",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
				return []byte(msg), errors.New("exit 1")
			})
			t.Cleanup(func() { setSystemctlRunner(prev) })

			if err := stopMailman(context.Background(), "ghost"); err != nil {
				t.Errorf("expected idempotent success, got %v", err)
			}
		})
	}
}

func TestStopMailman_RealErrorPropagates(t *testing.T) {
	prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
		return []byte("permission denied or whatever"), errors.New("exit 1")
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	if err := stopMailman(context.Background(), "foo"); err == nil {
		t.Error("want error for real failure")
	}
}
