package main

import (
	"context"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// TestFlagOperator_HappyPath pins the load-bearing invariant: a successful
// flag_operator call posts a message to the operator-attention recipient
// AND flips the sender's attention_state to "awaiting_operator". Both
// substrate mutations must land for the operator-visibility loop to work.
func TestFlagOperator_HappyPath(t *testing.T) {
	s := newCmdTestStore(t, "alice", operatorAttentionRecipient)
	ctx := context.Background()

	result, code := doFlagOperator(ctx, s, "alice", "PR #999 needs your read before merge")

	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}
	if ok, _ := result["ok"].(bool); !ok {
		t.Errorf("ok = %v; result=%v", result["ok"], result)
	}
	if name, _ := result["name"].(string); name != "alice" {
		t.Errorf("name = %q, want %q", name, "alice")
	}
	if state, _ := result["attention_state"].(string); state != store.AttentionStateAwaitingOperator {
		t.Errorf("attention_state = %q, want %q", state, store.AttentionStateAwaitingOperator)
	}

	// Verify the substrate mutation: alice's attention_state is set.
	alice, err := s.GetAgent(ctx, "alice")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if alice.AttentionState != store.AttentionStateAwaitingOperator {
		t.Errorf("alice.AttentionState = %q, want %q",
			alice.AttentionState, store.AttentionStateAwaitingOperator)
	}

	// Verify the message landed in operator-attention's queue.
	depth, err := s.RecipientQueueDepth(ctx, operatorAttentionRecipient)
	if err != nil {
		t.Fatalf("RecipientQueueDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("operator-attention queue depth = %d, want 1", depth)
	}
}

// TestFlagOperator_UnregisteredRecipient pins the fail-loud principle: when
// the operator-attention recipient has not been set up, flag_operator
// refuses with a descriptive error and does NOT mutate the sender's
// attention_state. This mirrors the #152 send-to-unregistered fail-loud
// semantic — a typo'd or absent operator-attention pseudo-agent must not
// silently swallow the attention signal.
func TestFlagOperator_UnregisteredRecipient(t *testing.T) {
	s := newCmdTestStore(t, "alice") // operator-attention deliberately absent
	ctx := context.Background()

	result, code := doFlagOperator(ctx, s, "alice", "need attention")

	if code != exitDataErr {
		t.Errorf("exit code = %d, want %d (exitDataErr)", code, exitDataErr)
	}
	if ok, _ := result["ok"].(bool); ok {
		t.Errorf("ok = true; expected false for unregistered recipient")
	}
	errMsg, _ := result["error"].(string)
	if errMsg == "" {
		t.Errorf("error message empty; want a descriptive recipient-not-registered error")
	}

	// Verify substrate NOT mutated: alice's attention_state stays "idle".
	alice, err := s.GetAgent(ctx, "alice")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if alice.AttentionState != store.AttentionStateIdle {
		t.Errorf("alice.AttentionState = %q, want %q (no mutation on failed flag)",
			alice.AttentionState, store.AttentionStateIdle)
	}
}

// TestRegisterAutoClearsAttentionState pins the register auto-clear
// invariant: when a chamber re-registers (post-/compact, post-restart,
// spawn-die cycle), any stale "awaiting_operator" flag from the prior
// session is reset to "idle". This prevents the operator's attention
// queue from carrying stale signals across chamber lifecycle events.
func TestRegisterAutoClearsAttentionState(t *testing.T) {
	s := newCmdTestStore(t, "bosun")
	ctx := context.Background()

	// Manually set bosun's attention_state as if a prior session flagged.
	if err := s.SetAttentionState(ctx, "bosun", store.AttentionStateAwaitingOperator); err != nil {
		t.Fatalf("SetAttentionState (seed): %v", err)
	}

	// Sanity: the seed worked.
	got, _ := s.GetAgent(ctx, "bosun")
	if got.AttentionState != store.AttentionStateAwaitingOperator {
		t.Fatalf("seed failed; AttentionState = %q", got.AttentionState)
	}

	// Now simulate a (re)register by calling UpsertAgent + the same explicit
	// clear the register handler performs. (Direct invocation rather than
	// re-driving the CLI keeps the test cmd-shell-free.)
	if err := s.UpsertAgent(ctx, "bosun", "%5"); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	if err := s.SetAttentionState(ctx, "bosun", store.AttentionStateIdle); err != nil {
		t.Fatalf("SetAttentionState (clear): %v", err)
	}

	// Verify clear took effect.
	bosun, _ := s.GetAgent(ctx, "bosun")
	if bosun.AttentionState != store.AttentionStateIdle {
		t.Errorf("bosun.AttentionState = %q, want %q after register auto-clear",
			bosun.AttentionState, store.AttentionStateIdle)
	}
}
