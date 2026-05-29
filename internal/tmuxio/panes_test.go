package tmuxio

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestLivePanes_Parses(t *testing.T) {
	prev := listPanesRunner
	t.Cleanup(func() { listPanesRunner = prev })
	listPanesRunner = func(_ context.Context) ([]byte, error) {
		return []byte("%1\n%3\n%5\n"), nil
	}

	got, err := LivePanes(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for _, want := range []string{"%1", "%3", "%5"} {
		if !got[want] {
			t.Errorf("missing %s", want)
		}
	}
	if len(got) != 3 {
		t.Errorf("len = %d, want 3", len(got))
	}
}

func TestLivePanes_NoServerTreatedAsEmpty(t *testing.T) {
	prev := listPanesRunner
	t.Cleanup(func() { listPanesRunner = prev })
	listPanesRunner = func(_ context.Context) ([]byte, error) {
		// Simulate "no server running" — tmux exits non-zero.
		return nil, &exec.ExitError{}
	}
	got, err := LivePanes(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestLivePanes_TmuxNotInstalled(t *testing.T) {
	prev := listPanesRunner
	t.Cleanup(func() { listPanesRunner = prev })
	listPanesRunner = func(_ context.Context) ([]byte, error) {
		return nil, &exec.Error{Name: "tmux", Err: errors.New("executable file not found")}
	}
	got, err := LivePanes(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestLivePanes_TrimsWhitespace(t *testing.T) {
	prev := listPanesRunner
	t.Cleanup(func() { listPanesRunner = prev })
	listPanesRunner = func(_ context.Context) ([]byte, error) {
		return []byte("  %1  \n\n  %3  \n"), nil
	}
	got, _ := LivePanes(context.Background())
	if !got["%1"] || !got["%3"] {
		t.Errorf("got %v", got)
	}
}
