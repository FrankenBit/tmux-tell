package cli

import (
	"context"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// seedQueued inserts n queued messages to `to` and returns the highest
// (newest) numeric id, so policy tests can assert the stamped floor.
func seedQueued(t *testing.T, s *store.Store, to string, n int) (maxID int64) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		r, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: "sender", ToAgent: to, Body: "queued",
		})
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		m, err := s.GetMessage(ctx, r.PublicID)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		maxID = m.ID
	}
	return maxID
}

// backlogAnnounces returns every KindBacklogAnnounce message addressed to
// `to` — the synthetic 📬 nudge rows the policy inserts.
func backlogAnnounces(t *testing.T, s *store.Store, to string) []store.Message {
	t.Helper()
	all, err := s.ListMessages(context.Background(), store.ListFilter{
		ToAgent: to, Kind: store.KindBacklogAnnounce,
	})
	if err != nil {
		t.Fatalf("list announces: %v", err)
	}
	return all
}

func strptr(s string) *string { return &s }
func intptr(n int) *int       { return &n }

func TestApplyBacklogPolicy_AnnounceDefault(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	ctx := context.Background()
	maxID := seedQueued(t, s, "bob", 3)

	// nil config → hardcoded default policy "announce".
	bp := applyBacklogPolicy(ctx, s, nil, "bob", store.DeliveryModePasteAndEnter, 3)
	if bp.Policy != config.BacklogAnnounce {
		t.Errorf("policy = %q, want announce", bp.Policy)
	}
	if bp.Skipped != 3 {
		t.Errorf("skipped = %d, want 3", bp.Skipped)
	}
	if bp.NudgeID == "" {
		t.Errorf("expected a nudge id, got empty")
	}
	if bp.Err != nil {
		t.Errorf("unexpected err: %v", bp.Err)
	}

	// Floor stamped at the highest queued id.
	a, _ := s.GetAgent(ctx, "bob")
	if !a.BacklogEpoch.Valid || a.BacklogEpoch.Int64 != maxID {
		t.Errorf("BacklogEpoch = %+v, want valid %d", a.BacklogEpoch, maxID)
	}

	// Exactly one nudge, with the skipped count in its body.
	ann := backlogAnnounces(t, s, "bob")
	if len(ann) != 1 {
		t.Fatalf("announces = %d, want 1", len(ann))
	}
	if ann[0].Body != "📬 3 queued — run tmux-msg.inbox" {
		t.Errorf("nudge body = %q", ann[0].Body)
	}
	if ann[0].FromAgent != "bob" || ann[0].ToAgent != "bob" {
		t.Errorf("nudge not self-addressed: %s→%s", ann[0].FromAgent, ann[0].ToAgent)
	}
}

func TestApplyBacklogPolicy_AutoDeliverWithinCap(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	ctx := context.Background()
	seedQueued(t, s, "bob", 3)

	cfg := &config.File{Agent: map[string]config.Block{
		"bob": {OnRegisterBacklog: strptr(config.BacklogAutoDeliver), OnRegisterBacklogCap: intptr(5)},
	}}
	bp := applyBacklogPolicy(ctx, s, cfg, "bob", store.DeliveryModePasteAndEnter, 3)
	if bp.Policy != config.BacklogAutoDeliver {
		t.Errorf("policy = %q, want auto-deliver", bp.Policy)
	}
	if bp.Skipped != 0 {
		t.Errorf("skipped = %d, want 0 (whole backlog within cap)", bp.Skipped)
	}
	if bp.NudgeID != "" {
		t.Errorf("nudge inserted when nothing skipped: %s", bp.NudgeID)
	}
	// Epoch untouched → mailman delivers all three normally.
	a, _ := s.GetAgent(ctx, "bob")
	if a.BacklogEpoch.Valid {
		t.Errorf("epoch stamped when nothing skipped: %+v", a.BacklogEpoch)
	}
	if len(backlogAnnounces(t, s, "bob")) != 0 {
		t.Errorf("nudge inserted when nothing skipped")
	}
}

func TestApplyBacklogPolicy_AutoDeliverExceedsCap(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	ctx := context.Background()
	seedQueued(t, s, "bob", 5)

	cfg := &config.File{Agent: map[string]config.Block{
		"bob": {OnRegisterBacklog: strptr(config.BacklogAutoDeliver), OnRegisterBacklogCap: intptr(2)},
	}}
	bp := applyBacklogPolicy(ctx, s, cfg, "bob", store.DeliveryModePasteAndEnter, 5)
	if bp.Skipped != 3 {
		t.Errorf("skipped = %d, want 3 (5 − cap 2)", bp.Skipped)
	}
	if bp.NudgeID == "" {
		t.Errorf("expected a nudge id")
	}
	a, _ := s.GetAgent(ctx, "bob")
	if !a.BacklogEpoch.Valid {
		t.Errorf("epoch not stamped")
	}
	ann := backlogAnnounces(t, s, "bob")
	if len(ann) != 1 || ann[0].Body != "📬 3 queued — run tmux-msg.inbox" {
		t.Errorf("nudge = %+v, want one body '📬 3 queued …'", ann)
	}
}

func TestApplyBacklogPolicy_MailboxOnlyNoOp(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	ctx := context.Background()
	seedQueued(t, s, "bob", 3)

	bp := applyBacklogPolicy(ctx, s, nil, "bob", store.DeliveryModeMailboxOnly, 3)
	if bp.Policy != "" {
		t.Errorf("policy = %q, want empty (mailbox-only no-op)", bp.Policy)
	}
	a, _ := s.GetAgent(ctx, "bob")
	if a.BacklogEpoch.Valid {
		t.Errorf("epoch stamped for mailbox-only agent: %+v", a.BacklogEpoch)
	}
	if len(backlogAnnounces(t, s, "bob")) != 0 {
		t.Errorf("nudge inserted for mailbox-only agent")
	}
}

func TestApplyBacklogPolicy_NoBacklogNoOp(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	bp := applyBacklogPolicy(context.Background(), s, nil, "bob", store.DeliveryModePasteAndEnter, 0)
	if bp.Policy != "" {
		t.Errorf("policy = %q, want empty (no backlog)", bp.Policy)
	}
}

func TestApplyBacklogPolicy_UnknownPolicyFallsBackToAnnounce(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	ctx := context.Background()
	seedQueued(t, s, "bob", 2)

	cfg := &config.File{Agent: map[string]config.Block{
		"bob": {OnRegisterBacklog: strptr("bogus-typo")},
	}}
	bp := applyBacklogPolicy(ctx, s, cfg, "bob", store.DeliveryModePasteAndEnter, 2)
	if bp.Policy != config.BacklogAnnounce {
		t.Errorf("policy = %q, want announce fallback", bp.Policy)
	}
	if bp.Skipped != 2 {
		t.Errorf("skipped = %d, want 2 (announce skips all)", bp.Skipped)
	}
}

func TestApplyBacklogPolicy_ReRegisterAdvancesFloor(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	ctx := context.Background()
	seedQueued(t, s, "bob", 2)

	// First register: announce → floor at the 2nd id.
	bp1 := applyBacklogPolicy(ctx, s, nil, "bob", store.DeliveryModePasteAndEnter, 2)
	a1, _ := s.GetAgent(ctx, "bob")
	floor1 := a1.BacklogEpoch.Int64
	if bp1.Skipped != 2 {
		t.Fatalf("first skipped = %d, want 2", bp1.Skipped)
	}

	// New mail arrives, then a re-register: the floor must advance past it
	// (the new arrival + the prior nudge are now backlog too).
	newMax := seedQueued(t, s, "bob", 2)
	depth, _ := s.RecipientQueueDepth(ctx, "bob")
	applyBacklogPolicy(ctx, s, nil, "bob", store.DeliveryModePasteAndEnter, depth)
	a2, _ := s.GetAgent(ctx, "bob")
	if a2.BacklogEpoch.Int64 <= floor1 {
		t.Errorf("floor did not advance: %d → %d", floor1, a2.BacklogEpoch.Int64)
	}
	if a2.BacklogEpoch.Int64 != newMax {
		t.Errorf("re-register floor = %d, want newest id %d", a2.BacklogEpoch.Int64, newMax)
	}
}

// TestMCP_Register_AnnouncesBacklog proves the MCP register surface wires the
// policy: a re-register with a queued backlog stamps the floor and surfaces
// the announce fields alongside the #151 queued count.
func TestMCP_Register_AnnouncesBacklog(t *testing.T) {
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("CLAUDE_MSG_CONFIG", "/nonexistent/tmux-msg.toml") // force default policy
	s := newCmdTestStore(t, "sender", "backlogged")
	(&fakeSystemctl{}).install(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: "sender", ToAgent: "backlogged", Body: "queued",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
		"name": "backlogged", "force": true,
	})
	if got["ok"] != true {
		t.Fatalf("got=%v", got)
	}
	if got["backlog_policy"] != "announce" {
		t.Errorf("backlog_policy = %v, want announce", got["backlog_policy"])
	}
	if int(got["backlog_skipped"].(float64)) != 3 {
		t.Errorf("backlog_skipped = %v, want 3", got["backlog_skipped"])
	}
	if got["backlog_nudge"] == nil || got["backlog_nudge"] == "" {
		t.Errorf("backlog_nudge missing: %v", got)
	}
	a, _ := s.GetAgent(ctx, "backlogged")
	if !a.BacklogEpoch.Valid {
		t.Errorf("epoch not stamped on MCP register")
	}
}

// TestMCP_Register_MailboxOnlyNoBacklogFields proves the policy is a no-op for
// mailbox-only agents even with a backlog present.
func TestMCP_Register_MailboxOnlyNoBacklogFields(t *testing.T) {
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("CLAUDE_MSG_CONFIG", "/nonexistent/tmux-msg.toml")
	s := newCmdTestStore(t, "sender", "mbox")
	(&fakeSystemctl{}).install(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "sender", ToAgent: "mbox", Body: "q"})
	}

	got := callMCPTool(t, s, "tmux-msg.register", map[string]any{
		"name": "mbox", "force": true, "delivery_mode": "mailbox-only",
	})
	if got["ok"] != true {
		t.Fatalf("got=%v", got)
	}
	if _, present := got["backlog_policy"]; present {
		t.Errorf("backlog_policy present for mailbox-only: %v", got)
	}
	// queued still surfaces (#151), it's just not auto-announced.
	if int(got["queued"].(float64)) != 2 {
		t.Errorf("queued = %v, want 2", got["queued"])
	}
	a, _ := s.GetAgent(ctx, "mbox")
	if a.BacklogEpoch.Valid {
		t.Errorf("epoch stamped for mailbox-only agent")
	}
}
