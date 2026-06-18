package store

import (
	"context"
	"errors"
	"testing"
)

// A freshly-registered agent has an empty display_name (the migration's
// default); SetDisplayName persists it and GetAgent / ListAgents read it back.
func TestSetDisplayName_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "lookout", "%6"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	a, err := s.GetAgent(ctx, "lookout")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.DisplayName != "" {
		t.Errorf("fresh agent display_name = %q, want empty", a.DisplayName)
	}

	if err := s.SetDisplayName(ctx, "lookout", "Lookout"); err != nil {
		t.Fatalf("SetDisplayName: %v", err)
	}
	a, err = s.GetAgent(ctx, "lookout")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.DisplayName != "Lookout" {
		t.Errorf("display_name = %q, want Lookout", a.DisplayName)
	}

	// Multi-word preserved verbatim, and the listing path carries it too.
	if err := s.SetDisplayName(ctx, "lookout", "Master Bosun"); err != nil {
		t.Fatalf("SetDisplayName multi-word: %v", err)
	}
	list, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, ag := range list {
		if ag.Name == "lookout" {
			found = true
			if ag.DisplayName != "Master Bosun" {
				t.Errorf("ListAgents display_name = %q, want \"Master Bosun\"", ag.DisplayName)
			}
		}
	}
	if !found {
		t.Fatal("lookout not in ListAgents")
	}
}

func TestSetDisplayName_UnknownAgent(t *testing.T) {
	s := newTestStore(t)
	err := s.SetDisplayName(context.Background(), "ghost", "Ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
