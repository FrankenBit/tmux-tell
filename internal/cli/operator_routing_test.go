package cli

import (
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestResolveOperatorTarget_AttachedAtChamber pins the load-bearing
// load-path: tmux reports a client active at a registered chamber's
// pane, so the resolver returns that chamber AND updates the presence
// slot so a later fallback knows where the operator was.
func TestResolveOperatorTarget_AttachedAtChamber(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()

	// Register alice at pane %5; bob at %7.
	_ = s.UpsertAgent(ctx, "alice", "%5")
	_ = s.UpsertAgent(ctx, "bob", "%7")

	// Fake tmux: operator attached to alice's pane %5.
	restore := tmuxio.SetListClientsRunner(func(ctx context.Context) ([]byte, error) {
		return []byte("%5\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListClientsRunner(restore) })

	target, err := resolveOperatorTarget(ctx, s)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target != "alice" {
		t.Errorf("target = %q, want %q", target, "alice")
	}

	// Verify the substrate's last-seen-in slot was updated.
	slot, err := s.GetPresence(ctx, store.PresenceKeyOperatorLastSeenIn)
	if err != nil {
		t.Fatalf("slot lookup: %v", err)
	}
	if slot != "alice" {
		t.Errorf("slot = %q, want %q", slot, "alice")
	}
}

// TestResolveOperatorTarget_FallbackToSlot pins the operator-not-at-
// chamber path: tmux reports no clients (operator left their pane), so
// the resolver falls back to the last-seen-in slot.
func TestResolveOperatorTarget_FallbackToSlot(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()

	_ = s.UpsertAgent(ctx, "bosun", "%3")
	_ = s.SetPresence(ctx, store.PresenceKeyOperatorLastSeenIn, "bosun")

	// Fake tmux: no clients attached.
	restore := tmuxio.SetListClientsRunner(func(ctx context.Context) ([]byte, error) {
		return []byte(""), nil
	})
	t.Cleanup(func() { tmuxio.SetListClientsRunner(restore) })

	target, err := resolveOperatorTarget(ctx, s)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target != "bosun" {
		t.Errorf("target = %q, want %q (slot fallback)", target, "bosun")
	}
}

// TestResolveOperatorTarget_SlotReferencesUnregistered pins the
// substrate-honest validation step: the slot's value might point at a
// chamber that has since been unregistered. In that case the resolver
// fails-loud rather than routing to a phantom name.
func TestResolveOperatorTarget_SlotReferencesUnregistered(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()

	// Slot points at "ghost" but ghost is not in the agents table.
	_ = s.SetPresence(ctx, store.PresenceKeyOperatorLastSeenIn, "ghost")

	restore := tmuxio.SetListClientsRunner(func(ctx context.Context) ([]byte, error) {
		return []byte(""), nil
	})
	t.Cleanup(func() { tmuxio.SetListClientsRunner(restore) })

	_, err := resolveOperatorTarget(ctx, s)
	if err == nil {
		t.Errorf("resolve to unregistered slot target should error; got nil")
	}
	// Pin substrate-honest error framing: the message must name the
	// "unregistered chamber" condition so the sender can distinguish
	// it from the never-observed case (Surveyor N1 on PR #257).
	if err != nil && !strings.Contains(err.Error(), "unregistered chamber") {
		t.Errorf("error should mention 'unregistered chamber'; got %q", err.Error())
	}
}

// TestResolveOperatorTarget_NeverObserved pins the bootstrap case: no
// active client, no slot record. The resolver fails-loud with a
// descriptive error so the sender knows the setup precondition.
func TestResolveOperatorTarget_NeverObserved(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()

	restore := tmuxio.SetListClientsRunner(func(ctx context.Context) ([]byte, error) {
		return []byte(""), nil
	})
	t.Cleanup(func() { tmuxio.SetListClientsRunner(restore) })

	_, err := resolveOperatorTarget(ctx, s)
	if err == nil {
		t.Errorf("resolve with no observation should error")
	}
	if err != nil && !strings.Contains(err.Error(), "not observed") {
		t.Errorf("error should mention 'not observed'; got %q", err.Error())
	}
}

// TestResolveOperatorInSendParams_SingleRecipient pins the single-To
// substitution: p.To == "operator" gets replaced by the resolved chamber
// name.
func TestResolveOperatorInSendParams_SingleRecipient(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()

	_ = s.UpsertAgent(ctx, "alice", "%5")
	restore := tmuxio.SetListClientsRunner(func(ctx context.Context) ([]byte, error) {
		return []byte("%5\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListClientsRunner(restore) })

	p := sendParams{To: OperatorRecipient}
	if err := resolveOperatorInSendParams(ctx, s, &p); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.To != "alice" {
		t.Errorf("p.To = %q, want %q", p.To, "alice")
	}
}

// TestResolveOperatorInSendParams_MultiRecipient pins the substitution
// in p.ToRecipients: only the "operator" entries get substituted; other
// recipients pass through unchanged.
func TestResolveOperatorInSendParams_MultiRecipient(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()

	_ = s.UpsertAgent(ctx, "alice", "%5")
	_ = s.UpsertAgent(ctx, "bob", "%7")
	_ = s.UpsertAgent(ctx, "carol", "%9")
	restore := tmuxio.SetListClientsRunner(func(ctx context.Context) ([]byte, error) {
		return []byte("%5\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListClientsRunner(restore) })

	p := sendParams{ToRecipients: []string{"bob", OperatorRecipient, "carol"}}
	if err := resolveOperatorInSendParams(ctx, s, &p); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"bob", "alice", "carol"}
	if len(p.ToRecipients) != len(want) {
		t.Fatalf("len = %d, want %d", len(p.ToRecipients), len(want))
	}
	for i := range want {
		if p.ToRecipients[i] != want[i] {
			t.Errorf("ToRecipients[%d] = %q, want %q", i, p.ToRecipients[i], want[i])
		}
	}
}

// TestResolveOperatorInSendParams_NoOperator pins the no-op invariant:
// when neither field contains "operator", the resolver doesn't touch
// the substrate (no tmux query, no slot lookup) — keeping the
// chamber-to-chamber hot path fast.
func TestResolveOperatorInSendParams_NoOperator(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()

	// Set the tmux runner to FAIL — proves the resolver doesn't call it.
	restore := tmuxio.SetListClientsRunner(func(ctx context.Context) ([]byte, error) {
		t.Errorf("tmux runner should not be called when no operator recipient")
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetListClientsRunner(restore) })

	p := sendParams{To: "alice"}
	if err := resolveOperatorInSendParams(ctx, s, &p); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.To != "alice" {
		t.Errorf("p.To = %q, want unchanged %q", p.To, "alice")
	}
}
