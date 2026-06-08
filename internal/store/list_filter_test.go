package store

import (
	"context"
	"strings"
	"testing"
)

// TestListMessages_UnverifiedValidation pins the fail-loud check added in
// #220: Unverified=true is only valid when State is empty or StateDelivered.
func TestListMessages_UnverifiedValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Unverified alone (State="") → valid.
	if _, err := s.ListMessages(ctx, ListFilter{Unverified: true}); err != nil {
		t.Errorf("Unverified alone should be valid; got %v", err)
	}

	// Unverified + State=StateDelivered → valid.
	if _, err := s.ListMessages(ctx, ListFilter{
		Unverified: true,
		State:      StateDelivered,
	}); err != nil {
		t.Errorf("Unverified + State=delivered should be valid; got %v", err)
	}

	// Unverified + State=StateQueued → error.
	_, err := s.ListMessages(ctx, ListFilter{Unverified: true, State: StateQueued})
	if err == nil {
		t.Fatal("Unverified + State=queued should error; got nil")
	}
	if !strings.Contains(err.Error(), "Unverified=true") {
		t.Errorf("error should mention Unverified=true; got %v", err)
	}
}
