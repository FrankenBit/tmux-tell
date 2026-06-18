package tmuxio

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSetPaneTitle_RunsSelectPane(t *testing.T) {
	var gotArgs []string
	restore := SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	})
	defer SetTmuxRunner(restore)

	if err := SetPaneTitle(context.Background(), "%5", "Shipwright"); err != nil {
		t.Fatalf("SetPaneTitle: %v", err)
	}
	want := []string{"select-pane", "-t", "%5", "-T", "Shipwright"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
}

// Multi-word titles must reach tmux as a SINGLE argv element so a name like
// "Master Bosun" is not split into two args (which select-pane would reject /
// misparse). The launch-path coverage matrix lists multi-word names as in
// scope (#556).
func TestSetPaneTitle_MultiWordSingleArg(t *testing.T) {
	var gotArgs []string
	restore := SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	})
	defer SetTmuxRunner(restore)

	if err := SetPaneTitle(context.Background(), "%5", "Master Bosun"); err != nil {
		t.Fatalf("SetPaneTitle: %v", err)
	}
	if len(gotArgs) != 5 || gotArgs[4] != "Master Bosun" {
		t.Errorf("multi-word title not passed as single arg: %#v", gotArgs)
	}
}

func TestSetPaneTitle_RejectsEmpty(t *testing.T) {
	restore := SetTmuxRunner(func(_ context.Context, _ io.Reader, _ ...string) ([]byte, error) {
		t.Fatal("tmux should not be invoked for an invalid request")
		return nil, nil
	})
	defer SetTmuxRunner(restore)

	if err := SetPaneTitle(context.Background(), "", "X"); err == nil {
		t.Error("empty pane should error")
	}
	if err := SetPaneTitle(context.Background(), "%5", ""); err == nil {
		t.Error("empty title should error")
	}
}

func TestSetPaneTitle_PropagatesTmuxError(t *testing.T) {
	restore := SetTmuxRunner(func(_ context.Context, _ io.Reader, _ ...string) ([]byte, error) {
		return []byte("can't find pane: %9"), errors.New("exit status 1")
	})
	defer SetTmuxRunner(restore)

	err := SetPaneTitle(context.Background(), "%9", "Ghost")
	if err == nil {
		t.Fatal("expected error when tmux fails")
	}
	if !strings.Contains(err.Error(), "select-pane") {
		t.Errorf("error should name the operation; got %v", err)
	}
}
