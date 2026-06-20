package store_test

import (
	"context"
	"errors"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestRecipientQueueCap_ProviderFloor is the behavioural pin for #412: the
// recipient-queue cap floors UP per the recipient's provider, enforced in the
// single in-transaction cap site. A codex recipient (provider "openai") accepts
// up to the floor (20) even when the sender passes the default cap (5); a claude
// recipient (provider "anthropic") keeps the passed cap; an unregistered-provider
// recipient keeps the passed cap (conservative).
func TestRecipientQueueCap_ProviderFloor(t *testing.T) {
	ctx := context.Background()

	// fill inserts up to n messages from "alice" to recipient at the passed cap,
	// returning how many were accepted before the first queue-full rejection.
	// MaxSenderBacklog is left 0 so only the recipient-queue cap is under test.
	fill := func(t *testing.T, s *store.Store, recipient string, n, passedCap int) int {
		t.Helper()
		accepted := 0
		for i := 0; i < n; i++ {
			_, err := s.InsertMessage(ctx, store.InsertParams{
				FromAgent:         "alice",
				ToAgent:           recipient,
				Body:              "x",
				MaxRecipientQueue: passedCap,
			})
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, store.ErrRecipientQueueFull):
				return accepted
			default:
				t.Fatalf("unexpected insert error: %v", err)
			}
		}
		return accepted
	}

	seed := func(t *testing.T, recipient, provider string) *store.Store {
		t.Helper()
		s := openFileStore(t)
		if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
			t.Fatalf("seed sender: %v", err)
		}
		if err := s.UpsertAgent(ctx, recipient, "%2"); err != nil {
			t.Fatalf("seed recipient: %v", err)
		}
		if provider != "" {
			if err := s.SetProvider(ctx, recipient, provider); err != nil {
				t.Fatalf("set provider: %v", err)
			}
		}
		return s
	}

	t.Run("codex_openai_floored_to_20", func(t *testing.T) {
		s := seed(t, "codexbot", "openai")
		// Sender passes the default cap 5; the openai floor (20) must override it.
		if got := fill(t, s, "codexbot", 25, 5); got != 20 {
			t.Errorf("codex accepted = %d, want 20 (provider floor overrides passed cap 5)", got)
		}
	})

	t.Run("claude_anthropic_keeps_passed_cap", func(t *testing.T) {
		s := seed(t, "clauded", "anthropic")
		if got := fill(t, s, "clauded", 25, 5); got != 5 {
			t.Errorf("claude accepted = %d, want 5 (no floor; passed cap stands)", got)
		}
	})

	t.Run("unknown_provider_keeps_passed_cap", func(t *testing.T) {
		s := seed(t, "mystery", "") // no provider set
		if got := fill(t, s, "mystery", 10, 5); got != 5 {
			t.Errorf("unknown-provider accepted = %d, want 5 (conservative)", got)
		}
	})

	t.Run("higher_passed_cap_honored_above_floor", func(t *testing.T) {
		// The floor is a max(), never a min: an explicit cap ABOVE the provider
		// floor is honored, not lowered to it. codex (floor 20) with a passed
		// cap of 25 accepts 25 — pins the upper direction of max(passed, floor).
		s := seed(t, "codexbot", "openai")
		if got := fill(t, s, "codexbot", 30, 25); got != 25 {
			t.Errorf("codex with passed cap 25 accepted = %d, want 25 (floor never lowers an explicit higher cap)", got)
		}
	})
}
