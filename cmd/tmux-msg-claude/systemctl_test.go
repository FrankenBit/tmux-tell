package main

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
