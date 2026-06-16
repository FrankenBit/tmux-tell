package cli

import (
	"context"
	"fmt"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// backlogPolicyResult describes what applyBacklogPolicy did, for surfacing
// in the register response. A zero value (Policy == "") means the policy did
// not apply — the agent had no backlog or is mailbox-only.
type backlogPolicyResult struct {
	// Policy is the resolved don't-flood policy ("announce" | "auto-deliver"),
	// or "" when the policy did not apply.
	Policy string
	// Skipped is how many queued rows the policy left in place (announced
	// rather than pasted). 0 when everything fit the auto-deliver cap.
	Skipped int
	// NudgeID is the public_id of the inserted 📬 nudge, or "" when no nudge
	// was inserted (Skipped == 0, or the insert soft-failed — see Err).
	NudgeID string
	// Err is a soft error: registration already succeeded, so a store hiccup
	// here is reported to the caller (which surfaces it as `backlog_error`)
	// rather than failing the register. When SetBacklogEpoch succeeded but
	// the nudge insert failed, the floor still stands and the #151 `queued`
	// count in the same response still tells the session it has mail waiting.
	Err error
}

// applyBacklogPolicy implements the #204 don't-flood behavior for a freshly
// (re)registered agent: it stamps the claim-floor (backlog_epoch_id) so the
// mailman skips the pre-existing backlog the operator's policy chose not to
// paste all at once, and inserts a single synthetic 📬 nudge naming how many
// messages were left queued.
//
// Two policies, resolved from the on-register-backlog TOML knob (per-agent >
// defaults > hardcoded "announce"):
//
//   - "announce": leave the entire backlog queued; the mailman delivers only
//     the nudge. Floor = the highest existing queued id.
//   - "auto-deliver": deliver the newest on-register-backlog-cap messages
//     (they outrank the floor) and announce the older remainder. When the
//     whole backlog fits the cap, nothing is skipped — no floor change, no
//     nudge.
//
// Only paste-and-enter agents are eligible: a mailbox-only agent never gets a
// paste, so flooding is impossible and a nudge would just sit queued (the
// #151 `queued` count already tells a mailbox-only operator the depth). The
// caller passes the already-computed #151 backlog depth as `queued`; when
// it's 0 the call is a no-op.
//
// An unrecognized on-register-backlog value falls back to "announce" — the
// never-floods safe default — rather than erroring the register.
func applyBacklogPolicy(ctx context.Context, s *store.Store, cfg *config.File, name, deliveryMode string, queued int) backlogPolicyResult {
	if queued <= 0 || deliveryMode != store.DeliveryModePasteAndEnter {
		return backlogPolicyResult{}
	}

	policy := config.ResolveString(cfg, name, "on-register-backlog", config.DefaultOnRegisterBacklog)
	keepNewest := 0
	if policy == config.BacklogAutoDeliver {
		keepNewest = config.ResolveInt(cfg, name, "on-register-backlog-cap", config.DefaultOnRegisterBacklogCap)
		if keepNewest < 0 {
			keepNewest = 0
		}
	} else {
		// Any value other than auto-deliver — including a typo'd policy —
		// resolves to announce, which leaves the whole backlog queued.
		policy = config.BacklogAnnounce
	}

	res := backlogPolicyResult{Policy: policy}
	floor, skipped, err := s.QueuedBacklogFloor(ctx, name, keepNewest)
	if err != nil {
		res.Err = err
		return res
	}
	res.Skipped = skipped
	if skipped <= 0 {
		// Everything is within the cap (or announce on an empty delta):
		// deliver it all. Leave the epoch untouched — new arrivals always
		// get ids above any prior floor, so a stale floor never re-skips
		// them — and insert no nudge.
		return res
	}

	if err := s.SetBacklogEpoch(ctx, name, floor); err != nil {
		res.Err = err
		return res
	}

	// The nudge is a self-addressed synthetic message inserted via the
	// cap-bypass InsertNotice path (the same single-writer-safe path the
	// failure-notice and stranded-draft kinds use): the register process
	// never pastes, it only enqueues a row the agent's own mailman delivers.
	// Its id is higher than every skipped row (and every kept row), so it
	// outranks the floor and the mailman delivers it last — a heads-up that
	// lands after any auto-delivered backlog.
	nudge, err := s.InsertNotice(ctx, store.InsertParams{
		FromAgent: name,
		ToAgent:   name,
		Kind:      store.KindBacklogAnnounce,
		Body:      fmt.Sprintf("📬 %d queued — run tmux-tell.inbox", skipped),
	})
	if err != nil {
		res.Err = err
		return res
	}
	res.NudgeID = nudge.PublicID
	return res
}

// addBacklogPolicyFields folds a backlogPolicyResult into a register
// response map, keeping the CLI and MCP register surfaces shape-aligned. A
// no-op result (Policy == "") adds nothing.
func addBacklogPolicyFields(out map[string]any, bp backlogPolicyResult) {
	if bp.Policy == "" {
		return
	}
	out["backlog_policy"] = bp.Policy
	if bp.Skipped > 0 {
		out["backlog_skipped"] = bp.Skipped
	}
	if bp.NudgeID != "" {
		out["backlog_nudge"] = bp.NudgeID
	}
	if bp.Err != nil {
		out["backlog_error"] = bp.Err.Error()
	}
}
