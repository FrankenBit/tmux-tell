package cli

import (
	"context"
	"errors"
	"fmt"
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
// detection helper treats the user-home default path as "no mismatch" —
// the common-case caller (CLAUDE_MSG_DB unset, --db unset, resolved to
// defaultDBLocation()) must not be refused.
func TestStartMailmanWouldMismatchSystemd_DefaultDBClean(t *testing.T) {
	mismatched, callerDB := startMailmanWouldMismatchSystemd(defaultDBLocation())
	if mismatched {
		t.Errorf("mismatched=true for the default DB path; should be false")
	}
	if callerDB != defaultDBLocation() {
		t.Errorf("callerDB=%q, want defaultDBLocation() %q", callerDB, defaultDBLocation())
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
		"/tmp/sandbox.db",   // caller's DB
		defaultDBLocation(), // user-home default DB
		"--start-mailman=false",
		"serve --agent alice",
	} {
		if !strings.Contains(msg, frag) {
			t.Errorf("error missing %q\nmsg=%s", frag, msg)
		}
	}
}

// TestStartMailmanMissingEnv_CompleteEnvClean confirms the #356 helper returns
// no missing vars when both required D-Bus/XDG vars are present.
func TestStartMailmanMissingEnv_CompleteEnvClean(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if missing := startMailmanMissingEnv(); len(missing) != 0 {
		t.Errorf("missing=%v for complete env; want empty", missing)
	}
}

// TestStartMailmanMissingEnv_MissingVarsFire confirms the #356 helper fires
// correctly when either or both required vars are absent.
func TestStartMailmanMissingEnv_MissingVarsFire(t *testing.T) {
	cases := []struct {
		dbus string
		xdg  string
		want []string
	}{
		{"", "", []string{"DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR"}},
		{"unix:path=/run/user/1000/bus", "", []string{"XDG_RUNTIME_DIR"}},
		{"", "/run/user/1000", []string{"DBUS_SESSION_BUS_ADDRESS"}},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("dbus=%q,xdg=%q", c.dbus, c.xdg), func(t *testing.T) {
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", c.dbus)
			t.Setenv("XDG_RUNTIME_DIR", c.xdg)
			got := startMailmanMissingEnv()
			if len(got) != len(c.want) {
				t.Errorf("missing=%v, want %v", got, c.want)
				return
			}
			for i, v := range c.want {
				if got[i] != v {
					t.Errorf("missing[%d]=%q, want %q", i, got[i], v)
				}
			}
		})
	}
}

// TestStartMailmanEnvError_Actionable pins the structure of the #356 caller-
// facing error: missing var names named, plus the foreground-serve recovery.
func TestStartMailmanEnvError_Actionable(t *testing.T) {
	msg := startMailmanEnvError("alice", []string{"DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR"})
	for _, frag := range []string{
		"DBUS_SESSION_BUS_ADDRESS",
		"XDG_RUNTIME_DIR",
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
