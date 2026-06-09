package cli

import (
	"context"
	"errors"
	"fmt"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// OperatorRecipient is the reserved sender-facing name for operator-presence
// routing (#228). A `to: "operator"` send resolves at delivery-prep time to
// the chamber the operator is currently attached to (or was last attached
// to). Substrate-honest: the operator is not a registered agent in the
// agents table; this name is a routing primitive interpreted by the
// send-side resolver below.
const OperatorRecipient = "operator"

// resolveOperatorTarget resolves the "operator" recipient string to the
// canonical name of the chamber the operator is currently or was most
// recently attached to (#228). Resolution chain:
//
//  1. Query tmux active-client panes. For each, look up a registered
//     agent whose pane_id matches. If any match, return THAT agent's
//     name AND update the presence slot so a later fallback knows where
//     the operator was.
//
//  2. If no live tmux client points at a registered chamber (operator
//     in their own shell, no client attached, etc.), fall back to the
//     presence slot — the last observed chamber the operator was at.
//
//  3. If neither step yields a target (substrate has never observed
//     operator-at-a-chamber), return an error so the sender knows the
//     reserved name can't be resolved yet — fail-loud, no silent drop
//     (matches the #152 send-to-unregistered semantic).
//
// The "active pane updates the slot" behavior is the load-bearing
// substrate-honesty piece: the slot is always the most-recently-
// observed truth, so a later send with no attached client still routes
// to where the operator was.
func resolveOperatorTarget(ctx context.Context, s *store.Store) (string, error) {
	// Step 1: tmux active-client panes, resolve via agents table.
	if target := observeOperatorAtChamber(ctx, s); target != "" {
		// Update the slot — best-effort. A failed update is non-fatal:
		// we still have a target for THIS send; the slot will be
		// updated again the next time we observe successfully.
		_ = s.SetPresence(ctx, store.PresenceKeyOperatorLastSeenIn, target)
		return target, nil
	}
	// Step 2: fall back to the last-seen-in slot.
	last, err := s.GetPresence(ctx, store.PresenceKeyOperatorLastSeenIn)
	if err == nil {
		// Validate the slot's target is still registered. A chamber
		// that has been unregistered shouldn't accept routing.
		if _, gerr := s.GetAgent(ctx, last); gerr == nil {
			return last, nil
		}
		// Slot exists but points at an unregistered chamber. Substrate-
		// honest fail-loud rather than falling through to the
		// never-observed branch (which would wrap a nil error and emit
		// "%!w(<nil>)") — the operator was once at that chamber, but
		// the chamber is gone.
		return "", fmt.Errorf("operator presence slot points at unregistered chamber %q — re-attach to a registered chamber pane to re-seed the slot", last)
	}
	// Step 3: fail-loud.
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("operator presence not observed yet — no `last seen in` slot recorded. Substrate observes the operator the first time they're attached to a registered chamber pane (via `tmux list-clients`); until that observation lands, the `operator` recipient cannot be resolved")
	}
	return "", fmt.Errorf("operator presence lookup: %w", err)
}

// resolveOperatorInSendParams substitutes any "operator" entry in p.To /
// p.ToRecipients with the resolved chamber name (#228). Mutates p in
// place. Returns an error if any entry needs resolution and the substrate
// can't resolve it — sender sees the resolution failure rather than
// silently routing to nowhere.
//
// No-op when neither field contains "operator" — same allocations as the
// no-routing path, which is the hot path for chamber-to-chamber traffic.
func resolveOperatorInSendParams(ctx context.Context, s *store.Store, p *sendParams) error {
	if p.To != OperatorRecipient && !sliceContains(p.ToRecipients, OperatorRecipient) {
		return nil
	}
	target, err := resolveOperatorTarget(ctx, s)
	if err != nil {
		return err
	}
	if p.To == OperatorRecipient {
		p.To = target
	}
	for i, r := range p.ToRecipients {
		if r == OperatorRecipient {
			p.ToRecipients[i] = target
		}
	}
	return nil
}

// sliceContains reports whether needle is in haystack. Trivial helper;
// generic when we eventually go on Go 1.21+ slices.Contains.
func sliceContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// observeOperatorAtChamber returns the canonical name of the registered
// chamber whose pane_id matches an active tmux client pane, or "" if no
// such match. Substrate-honest: returns the FIRST match in tmux's
// list-clients output (single-operator-per-session per Q2 dissolution).
func observeOperatorAtChamber(ctx context.Context, s *store.Store) string {
	panes, err := tmuxio.ActiveClientPanes(ctx)
	if err != nil || len(panes) == 0 {
		return ""
	}
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return ""
	}
	// Build pane→name index once (small N; no need to be clever).
	byPane := make(map[string]string, len(agents))
	for _, a := range agents {
		if a.PaneID != "" {
			byPane[a.PaneID] = a.Name
		}
	}
	for _, pane := range panes {
		if name, ok := byPane[pane]; ok {
			return name
		}
	}
	return ""
}
