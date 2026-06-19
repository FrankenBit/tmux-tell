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
