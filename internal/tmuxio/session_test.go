package tmuxio

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestPaneCurrentPath(t *testing.T) {
	restore := SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		want := []string{"display-message", "-p", "-t", "%9", "#{pane_current_path}"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
		return []byte("/srv/codex/carpenter\n"), nil
	})
	t.Cleanup(func() { SetTmuxRunner(restore) })

	got, err := PaneCurrentPath(context.Background(), "%9")
	if err != nil {
		t.Fatalf("PaneCurrentPath: %v", err)
	}
	if got != "/srv/codex/carpenter" {
		t.Errorf("path = %q, want /srv/codex/carpenter", got)
	}
}

func TestPaneCurrentPathRejectsEmptyPane(t *testing.T) {
	if _, err := PaneCurrentPath(context.Background(), ""); err == nil {
		t.Fatal("PaneCurrentPath empty pane err = nil, want error")
	}
}

func TestPaneCurrentPathPropagatesTmuxError(t *testing.T) {
	restore := SetTmuxRunner(func(_ context.Context, _ io.Reader, _ ...string) ([]byte, error) {
		return []byte("no such pane"), errors.New("exit status 1")
	})
	t.Cleanup(func() { SetTmuxRunner(restore) })

	if _, err := PaneCurrentPath(context.Background(), "%404"); err == nil {
		t.Fatal("PaneCurrentPath tmux failure err = nil, want error")
	}
}

func TestPaneCurrentCommand(t *testing.T) {
	restore := SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		want := []string{"display-message", "-p", "-t", "%5", "#{pane_current_command}"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
		return []byte("claude\n"), nil
	})
	t.Cleanup(func() { SetTmuxRunner(restore) })

	got, err := PaneCurrentCommand(context.Background(), "%5")
	if err != nil {
		t.Fatalf("PaneCurrentCommand: %v", err)
	}
	if got != "claude" {
		t.Errorf("cmd = %q, want claude", got)
	}
	if _, err := PaneCurrentCommand(context.Background(), ""); err == nil {
		t.Fatal("PaneCurrentCommand empty pane err = nil, want error")
	}
}

// IsShellProcess is the #285/#730 "has the adapter exited to a bare shell?"
// classifier. It must positively match the interactive shells a chamber pane
// falls back to (incl. tmux's '-'-stripped login-shell form) and reject adapter /
// runtime process names — a false positive would send-keys the launch command into
// a live adapter.
func TestIsShellProcess(t *testing.T) {
	for _, sh := range []string{"bash", "-bash", "sh", "zsh", "fish", "dash", "ksh", "ash", "tcsh", "csh"} {
		if !IsShellProcess(sh) {
			t.Errorf("IsShellProcess(%q) = false, want true", sh)
		}
	}
	for _, notShell := range []string{"claude", "codex", "node", "aichat", "python", "", "vim", "bashful"} {
		if IsShellProcess(notShell) {
			t.Errorf("IsShellProcess(%q) = true, want false", notShell)
		}
	}
}

func TestRespawnPane(t *testing.T) {
	restore := SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		want := []string{"respawn-pane", "-k", "-t", "%9", "cd '/tmp' && exec '/srv/scripts/chamber-codex.sh' 'Carpenter'"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
		return nil, nil
	})
	t.Cleanup(func() { SetTmuxRunner(restore) })

	if err := RespawnPane(context.Background(), "%9", "cd '/tmp' && exec '/srv/scripts/chamber-codex.sh' 'Carpenter'"); err != nil {
		t.Fatalf("RespawnPane: %v", err)
	}
}

func TestRespawnPaneRejectsMissingInputs(t *testing.T) {
	if err := RespawnPane(context.Background(), "", "cmd"); err == nil {
		t.Fatal("RespawnPane empty pane err = nil, want error")
	}
	if err := RespawnPane(context.Background(), "%9", ""); err == nil {
		t.Fatal("RespawnPane empty command err = nil, want error")
	}
}
