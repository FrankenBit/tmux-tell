package cli

import (
	"context"
	"fmt"
	"time"

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
	// OldestAge is how long the oldest still-pending REAL deliverable has
	// been waiting, and OldestAgeOK reports whether it could be determined.
	// tmux-tell#934: "📬 N queued" reads identically on the first register and
	// the fifth, so a chamber cannot tell today's traffic from a row that has
	// been buried for a day. Zero-with-OK-false means could-not-tell, never
	// "brand new".
	OldestAge   time.Duration
	OldestAgeOK bool
	// PriorAnnounces is how many backlog_announce nudges were ALREADY
	// delivered to this agent before this one. >0 means the chamber has been
	// told before and the backlog is still unread — which is the epoch-ratchet
	// signature, since each register re-floors past the undelivered rows and
	// inserts a fresh nudge that clears the new floor.
	PriorAnnounces int
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
	// #934: age and prior-announce count, both SOFT — a store hiccup here
	// degrades the nudge to its pre-#934 wording rather than failing a
	// register that has already succeeded. res.Err is deliberately NOT set:
	// the caller surfaces that as `backlog_error`, and a missing cosmetic
	// suffix is not an error the operator needs to see.
	age, priorAnnounces, ageOK, ageErr := s.BacklogAgeAndAnnounces(ctx, name, time.Now())
	if ageErr == nil {
		res.OldestAge, res.OldestAgeOK, res.PriorAnnounces = age, ageOK, priorAnnounces
	}

	nudge, err := s.InsertNotice(ctx, store.InsertParams{
		FromAgent: name,
		ToAgent:   name,
		Kind:      store.KindBacklogAnnounce,
		Body:      backlogNudgeBody(skipped, res.OldestAge, res.OldestAgeOK, res.PriorAnnounces),
	})
	if err != nil {
		res.Err = err
		return res
	}
	res.NudgeID = nudge.PublicID
	return res
}

// backlogAgeFloor is the age below which the nudge omits the age clause —
// see backlogNudgeBody. One minute: anything fresher is the same register's
// own traffic, and every pre-#934 test fixture seeds milliseconds-old rows,
// so their bodies are unchanged by this feature rather than needing edits.
const backlogAgeFloor = time.Minute

// backlogNudgeBody renders the 📬 nudge.
//
// tmux-tell#934. The pre-#934 body was "📬 N queued — run tmux-tell.inbox",
// which is identical on the first register and the fifth. Two suffixes make
// the difference legible AT THE MOMENT THE NUDGE IS READ, which is the only
// moment a chamber is looking:
//
//	📬 2 queued — run tmux-tell.inbox                              first time, age unknown
//	📬 2 queued, oldest 32h — run tmux-tell.inbox                  age known
//	📬 2 queued, oldest 32h, announced 2× before — run …           told before, still unread
//
// The "announced N× before" clause is the epoch-ratchet tell: each register
// re-floors past the undelivered rows and inserts a nudge above the new floor,
// so a repeat count means the chamber has been notified and the rows are STILL
// buried. Without it, nudge five is indistinguishable from nudge one.
//
// Both clauses are omitted when unknown rather than rendered as zero — "oldest
// 0s" would assert the backlog just arrived, which is the opposite of what a
// could-not-tell means.
func backlogNudgeBody(skipped int, age time.Duration, ageOK bool, priorAnnounces int) string {
	b := fmt.Sprintf("📬 %d queued", skipped)
	// Below the floor the age is NOISE, not signal: a backlog seconds old is
	// today's traffic and the count already says everything. Rendering
	// "oldest 0s" on a fresh register would also assert the opposite of what
	// this surface exists to convey. The clause appears only once the age is
	// something a reader would act differently on.
	if ageOK && age >= backlogAgeFloor {
		b += fmt.Sprintf(", oldest %s", humanBacklogAge(age))
	}
	if priorAnnounces > 0 {
		b += fmt.Sprintf(", announced %d× before", priorAnnounces)
	}
	return b + " — run tmux-tell.inbox"
}

// humanBacklogAge renders a coarse age for the nudge: seconds under a minute,
// then minutes, then hours, then days. Deliberately coarse — the nudge is read
// in a pane and the actionable distinction is "minutes" versus "a day", not
// the exact figure.
func humanBacklogAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
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
	// #934: the machine-readable half of the nudge's new suffixes, for a
	// caller that reads the register response rather than the pane. Omitted
	// when unknown/zero rather than emitted as 0 — see backlogNudgeBody.
	if bp.OldestAgeOK {
		out["backlog_oldest_age_seconds"] = int(bp.OldestAge.Seconds())
	}
	if bp.PriorAnnounces > 0 {
		out["backlog_announced_before"] = bp.PriorAnnounces
	}
	if bp.Err != nil {
		out["backlog_error"] = bp.Err.Error()
	}
}
