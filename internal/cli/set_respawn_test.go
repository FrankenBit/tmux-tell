package cli

import (
	"context"
	"errors"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

func TestSetRespawnAfterShrinks_Core(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // best-effort close in test
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "pilot", "%6"); err != nil {
		t.Fatal(err)
	}

	// Round-trips to the store for a named target.
	res, err := setRespawnAfterShrinks(ctx, s, "pilot", 3)
	if err != nil {
		t.Fatalf("setRespawnAfterShrinks: %v", err)
	}
	if !res.OK || res.Agent != "pilot" || res.RespawnAfterShrinks != 3 {
		t.Errorf("result = %+v, want {OK:true Agent:pilot RespawnAfterShrinks:3}", res)
	}
	a, _ := s.GetAgent(ctx, "pilot")
	if a.RespawnAfterShrinks != 3 {
		t.Errorf("persisted respawn_after_shrinks = %d, want 3", a.RespawnAfterShrinks)
	}

	// Empty target is rejected at the surface (before the store) — the caller
	// must pass --name or run inside a registered pane.
	if _, err := setRespawnAfterShrinks(ctx, s, "", 3); err == nil {
		t.Error("empty target accepted, want an error")
	}

	// A missing target surfaces ErrNotFound from the store.
	if _, err := setRespawnAfterShrinks(ctx, s, "ghost", 3); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
