package cli

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

func deadMailmanNotices(t *testing.T, s *store.Store) []store.Message {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), store.ListFilter{Kind: store.KindDeadMailmanNotice})
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestObserveMailmen_InactiveRequiresTwoSamplesAndLatches(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bosun")
	if err := s.SetDeliveryMode(context.Background(), "bosun", store.DeliveryModeMailboxOnly); err != nil {
		t.Fatal(err)
	}
	restore := setSystemctlRunner(func(context.Context, ...string) ([]byte, error) {
		return []byte("ActiveState=failed\nUnitFileState=enabled\nNRestarts=5\nResult=oom-kill\n"), nil
	})
	t.Cleanup(func() { setSystemctlRunner(restore) })

	episodes := map[string]*mailmanEpisode{}
	var logs bytes.Buffer
	if err := observeMailmenSweep(context.Background(), s, "bosun", episodes, &logs); err != nil {
		t.Fatal(err)
	}
	if got := len(deadMailmanNotices(t, s)); got != 0 {
		t.Fatalf("first sample notices=%d, want 0", got)
	}
	if err := observeMailmenSweep(context.Background(), s, "bosun", episodes, &logs); err != nil {
		t.Fatal(err)
	}
	if err := observeMailmenSweep(context.Background(), s, "bosun", episodes, &logs); err != nil {
		t.Fatal(err)
	}
	if got := len(deadMailmanNotices(t, s)); got != 1 {
		t.Fatalf("latched notices=%d, want 1", got)
	}
}

func TestObserveMailmen_DisabledUnitIsLegitimateShutdown(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bosun")
	if err := s.SetDeliveryMode(context.Background(), "bosun", store.DeliveryModeMailboxOnly); err != nil {
		t.Fatal(err)
	}
	restore := setSystemctlRunner(func(context.Context, ...string) ([]byte, error) {
		return []byte("ActiveState=inactive\nUnitFileState=disabled\nNRestarts=0\nResult=success\n"), nil
	})
	t.Cleanup(func() { setSystemctlRunner(restore) })
	episodes := map[string]*mailmanEpisode{}
	for range 3 {
		if err := observeMailmenSweep(context.Background(), s, "bosun", episodes, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(deadMailmanNotices(t, s)); got != 0 {
		t.Fatalf("notices=%d, want 0", got)
	}
}

func TestObserveMailmen_RestartLoopDeltaAlerts(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bosun")
	if err := s.SetDeliveryMode(context.Background(), "bosun", store.DeliveryModeMailboxOnly); err != nil {
		t.Fatal(err)
	}
	restarts := 1
	restore := setSystemctlRunner(func(context.Context, ...string) ([]byte, error) {
		return []byte(fmt.Sprintf("ActiveState=active\nUnitFileState=enabled\nNRestarts=%d\nResult=success\n", restarts)), nil
	})
	t.Cleanup(func() { setSystemctlRunner(restore) })
	episodes := map[string]*mailmanEpisode{}
	if err := observeMailmenSweep(context.Background(), s, "bosun", episodes, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	restarts = 4
	if err := observeMailmenSweep(context.Background(), s, "bosun", episodes, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := len(deadMailmanNotices(t, s)); got != 1 {
		t.Fatalf("notices=%d, want 1", got)
	}
}
