package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open in-memory store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_AppliesSchemaIdempotently(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Schema is applied; tables exist.
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetAgent(ctx, "alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "alice" || got.PaneID != "%1" || got.Paused {
		t.Errorf("got %#v, want alice/%%1/paused=false", got)
	}

	// Re-applying the embedded schema is a no-op (no error).
	if _, err := s.DB().ExecContext(ctx, schemaSQL); err != nil {
		t.Errorf("schema re-apply: %v", err)
	}
}

func TestInsertMessage_HappyPath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	res, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: "alice",
		ToAgent:   "bob",
		Body:      "hello",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if len(res.PublicID) != 4 {
		t.Errorf("public_id length = %d, want 4", len(res.PublicID))
	}
	if res.Queued != 1 {
		t.Errorf("queued = %d, want 1", res.Queued)
	}
	got, err := s.GetMessage(ctx, res.PublicID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body != "hello" || got.FromAgent != "alice" || got.ToAgent != "bob" {
		t.Errorf("got %#v, want alice→bob/hello", got)
	}
	if got.State != StateQueued {
		t.Errorf("state = %s, want %s", got.State, StateQueued)
	}
	if got.CreatedAt == "" {
		t.Error("created_at empty")
	}
}

func TestInsertMessage_ValidatesInput(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cases := []struct {
		name string
		p    InsertParams
		want string
	}{
		{"empty body", InsertParams{FromAgent: "a", ToAgent: "b"}, "body"},
		{"empty from", InsertParams{ToAgent: "b", Body: "x"}, "from"},
		{"empty to", InsertParams{FromAgent: "a", Body: "x"}, "to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.InsertMessage(ctx, tc.p)
			if err == nil {
				t.Fatalf("got nil, want error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestInsertMessage_ReplyToMustExist(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: "alice",
		ToAgent:   "bob",
		ReplyTo:   "ffff",
		Body:      "hi",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInsertMessage_QueuedReflectsRecipientOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	r1, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	r2, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "carol", Body: "2"})
	r3, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "3"})

	// bob's queue grows independently of carol's.
	if r1.Queued != 1 || r2.Queued != 1 || r3.Queued != 2 {
		t.Errorf("queued depths = %d/%d/%d, want 1/1/2",
			r1.Queued, r2.Queued, r3.Queued)
	}
}

func TestClaimNext_FIFO(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ids := []string{}
	for i := 0; i < 3; i++ {
		r, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "m",
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		ids = append(ids, r.PublicID)
	}

	for i, want := range ids {
		m, err := s.ClaimNext(ctx, "bob")
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if m == nil {
			t.Fatalf("claim %d: nil message", i)
		}
		if m.PublicID != want {
			t.Errorf("claim %d = %s, want %s (FIFO violation)", i, m.PublicID, want)
		}
		if m.State != StateDelivering {
			t.Errorf("claim %d state = %s, want delivering", i, m.State)
		}
	}
	if m, err := s.ClaimNext(ctx, "bob"); err != nil || m != nil {
		t.Errorf("past-end claim = (%v, %v), want (nil, nil)", m, err)
	}
}

// TestClaimNext_NoReplyExpectedRoundTrip pins the scan of the no_reply_expected
// column in ClaimNext (#220 S1): a message inserted with NoReplyExpected=true
// must arrive with that field set after the claim transition.
func TestClaimNext_NoReplyExpectedRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("upsert alice: %v", err)
	}
	if err := s.UpsertAgent(ctx, "bob", "%2"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}

	_, err := s.InsertMessage(ctx, InsertParams{
		FromAgent:       "alice",
		ToAgent:         "bob",
		Body:            "fyi",
		NoReplyExpected: true,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	m, err := s.ClaimNext(ctx, "bob")
	if err != nil || m == nil {
		t.Fatalf("claim: m=%v err=%v", m, err)
	}
	if !m.NoReplyExpected {
		t.Errorf("NoReplyExpected = false after claim; want true (scan regression)")
	}
}

func TestClaimNext_ScopedToRecipient(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	bRes, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "for bob"})
	cRes, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "carol", Body: "for carol"})

	mb, _ := s.ClaimNext(ctx, "bob")
	if mb == nil || mb.PublicID != bRes.PublicID {
		t.Errorf("bob claim = %v, want %s", mb, bRes.PublicID)
	}
	mc, _ := s.ClaimNext(ctx, "carol")
	if mc == nil || mc.PublicID != cRes.PublicID {
		t.Errorf("carol claim = %v, want %s", mc, cRes.PublicID)
	}
}

func TestMarkDelivered(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "x"})
	if _, err := s.ClaimNext(ctx, "b"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.MarkDelivered(ctx, r.PublicID); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	got, _ := s.GetMessage(ctx, r.PublicID)
	if got.State != StateDelivered {
		t.Errorf("state = %s, want delivered", got.State)
	}
	if !got.DeliveredAt.Valid || got.DeliveredAt.String == "" {
		t.Errorf("delivered_at not set: %v", got.DeliveredAt)
	}
}

func TestMarkDelivered_RequiresDelivering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "x"})
	// not claimed → not delivering
	err := s.MarkDelivered(ctx, r.PublicID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMarkFailed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "x"})
	_, _ = s.ClaimNext(ctx, "b")
	if err := s.MarkFailed(ctx, r.PublicID, "tmux not responding"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, _ := s.GetMessage(ctx, r.PublicID)
	if got.State != StateFailed {
		t.Errorf("state = %s, want failed", got.State)
	}
	if !got.Error.Valid || got.Error.String != "tmux not responding" {
		t.Errorf("error = %v, want 'tmux not responding'", got.Error)
	}
}

func TestRecoverDelivering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "1"})
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "2"})
	_, _ = s.ClaimNext(ctx, "b")
	_, _ = s.ClaimNext(ctx, "b")
	// Both messages are 'delivering' now; queue depth is 0.
	if d, _ := s.RecipientQueueDepth(ctx, "b"); d != 0 {
		t.Errorf("pre-recover depth = %d, want 0", d)
	}

	n, err := s.RecoverDelivering(ctx, "b")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 2 {
		t.Errorf("recovered = %d, want 2", n)
	}
	if d, _ := s.RecipientQueueDepth(ctx, "b"); d != 2 {
		t.Errorf("post-recover depth = %d, want 2", d)
	}
}

func TestRecoverDelivering_ScopedToRecipient(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "1"})
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "c", Body: "2"})
	_, _ = s.ClaimNext(ctx, "b")
	_, _ = s.ClaimNext(ctx, "c")

	n, _ := s.RecoverDelivering(ctx, "b")
	if n != 1 {
		t.Errorf("recovered for b = %d, want 1", n)
	}
	if d, _ := s.RecipientQueueDepth(ctx, "c"); d != 0 {
		t.Errorf("c still has delivering, depth = %d, want 0", d)
	}
}

func TestQueueDepthAndBacklog(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "2"})
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "carol", ToAgent: "bob", Body: "3"})

	if d, _ := s.RecipientQueueDepth(ctx, "bob"); d != 3 {
		t.Errorf("bob depth = %d, want 3", d)
	}
	if b, _ := s.SenderBacklog(ctx, "alice"); b != 2 {
		t.Errorf("alice backlog = %d, want 2", b)
	}
	if b, _ := s.SenderBacklog(ctx, "carol"); b != 1 {
		t.Errorf("carol backlog = %d, want 1", b)
	}

	// Claiming reduces depth but not backlog (delivering ≠ queued).
	_, _ = s.ClaimNext(ctx, "bob")
	if d, _ := s.RecipientQueueDepth(ctx, "bob"); d != 2 {
		t.Errorf("after claim, bob depth = %d, want 2", d)
	}
}

// TestSenderBacklogCap_ScopedPerRecipient pins the #296 invariant at the
// store layer (where the cap predicate lives): the sender-backlog cap
// counts queued rows for the (from_agent, to_agent) pair, not globally
// per sender. A sender saturated at one recipient can still reach
// another. Mutation check: drop the `AND to_agent = ?` predicate from
// checkCapsInTx and the final alice→carol insert fails with
// ErrSenderBacklogFull.
func TestSenderBacklogCap_ScopedPerRecipient(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Saturate alice→bob at MaxSenderBacklog=2.
	for i := 0; i < 2; i++ {
		if _, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "m", MaxSenderBacklog: 2,
		}); err != nil {
			t.Fatalf("alice→bob #%d: %v", i, err)
		}
	}
	// A 3rd alice→bob is over the per-recipient cap.
	if _, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "m", MaxSenderBacklog: 2,
	}); !errors.Is(err, ErrSenderBacklogFull) {
		t.Fatalf("alice→bob #3 err = %v, want ErrSenderBacklogFull", err)
	}
	// But alice→carol is a different (sender, recipient) pair — bob's
	// saturated queue must not block it.
	if _, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: "alice", ToAgent: "carol", Body: "m", MaxSenderBacklog: 2,
	}); err != nil {
		t.Errorf("alice→carol err = %v, want nil (per-recipient scope must not block)", err)
	}
}

func TestReplyTo_StoresAndReturns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	orig, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "ping"})
	reply, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: "bob",
		ToAgent:   "alice",
		ReplyTo:   orig.PublicID,
		Body:      "pong",
	})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	got, _ := s.GetMessage(ctx, reply.PublicID)
	if !got.ReplyTo.Valid || got.ReplyTo.String != orig.PublicID {
		t.Errorf("reply_to = %v, want %s", got.ReplyTo, orig.PublicID)
	}
}

func TestGetMessage_NotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetMessage(context.Background(), "deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListMessages_FilterByEverything(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "carol", Body: "2"})
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "dave", ToAgent: "bob", Body: "3"})
	// Claim one to test state filtering.
	m, _ := s.ClaimNext(ctx, "bob")

	all, _ := s.ListMessages(ctx, ListFilter{})
	if len(all) != 3 {
		t.Errorf("all = %d, want 3", len(all))
	}
	toBob, _ := s.ListMessages(ctx, ListFilter{ToAgent: "bob"})
	if len(toBob) != 2 {
		t.Errorf("to bob = %d, want 2", len(toBob))
	}
	fromAlice, _ := s.ListMessages(ctx, ListFilter{FromAgent: "alice"})
	if len(fromAlice) != 2 {
		t.Errorf("from alice = %d, want 2", len(fromAlice))
	}
	queued, _ := s.ListMessages(ctx, ListFilter{State: StateQueued})
	if len(queued) != 2 {
		t.Errorf("queued = %d, want 2", len(queued))
	}
	delivering, _ := s.ListMessages(ctx, ListFilter{State: StateDelivering})
	if len(delivering) != 1 || delivering[0].PublicID != m.PublicID {
		t.Errorf("delivering = %v, want only %s", delivering, m.PublicID)
	}
}

func TestListMessages_LimitClamped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "x"})
	}
	got, _ := s.ListMessages(ctx, ListFilter{Limit: 3})
	if len(got) != 3 {
		t.Errorf("limit=3 → %d rows, want 3", len(got))
	}
}

func TestUpsertAgent_PreservesPaneIDOnEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if err := s.UpsertAgent(ctx, "alice", ""); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	a, _ := s.GetAgent(ctx, "alice")
	if a.PaneID != "%1" {
		t.Errorf("pane_id = %q, want preserved %%1", a.PaneID)
	}
}

func TestUpsertAgent_UpdatesPaneID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "alice", "%5")
	a, _ := s.GetAgent(ctx, "alice")
	if a.PaneID != "%5" {
		t.Errorf("pane_id = %q, want updated to %%5", a.PaneID)
	}
}

func TestSetPaused(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")

	if err := s.SetPaused(ctx, "alice", true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if a, _ := s.GetAgent(ctx, "alice"); !a.Paused {
		t.Errorf("paused = false after pause(true)")
	}
	if err := s.SetPaused(ctx, "alice", false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if a, _ := s.GetAgent(ctx, "alice"); a.Paused {
		t.Errorf("paused = true after pause(false)")
	}
}

func TestSetPaused_NotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetPaused(context.Background(), "ghost", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSetPausedAll(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%2")
	_ = s.UpsertAgent(ctx, "carol", "%3")

	n, err := s.SetPausedAll(ctx, true)
	if err != nil {
		t.Fatalf("pause all: %v", err)
	}
	if n != 3 {
		t.Errorf("affected = %d, want 3", n)
	}
	list, _ := s.ListAgents(ctx)
	for _, a := range list {
		if !a.Paused {
			t.Errorf("%s paused = false", a.Name)
		}
	}
}

func TestGetAgent_NotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetAgent(context.Background(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListAgents_OrderedByName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "charlie", "%3")
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%2")

	list, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	want := []string{"alice", "bob", "charlie"}
	for i, a := range list {
		if a.Name != want[i] {
			t.Errorf("[%d] = %s, want %s", i, a.Name, want[i])
		}
	}
}

func TestListAgents_NullPaneIDDecodesEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "")
	a, _ := s.GetAgent(ctx, "alice")
	if a.PaneID != "" {
		t.Errorf("pane_id = %q, want empty", a.PaneID)
	}
}

func TestSetAliases_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bosun", "%2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetAliases(ctx, "bosun", []string{"Master Bosun of Nimbus"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	a, err := s.GetAgent(ctx, "bosun")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(a.Aliases) != 1 || a.Aliases[0] != "Master Bosun of Nimbus" {
		t.Errorf("aliases = %v, want one entry", a.Aliases)
	}
}

func TestAddAlias_Idempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bosun", "%2")

	if err := s.AddAlias(ctx, "bosun", "MBoN"); err != nil {
		t.Fatalf("add 1: %v", err)
	}
	if err := s.AddAlias(ctx, "bosun", "MBoN"); err != nil {
		t.Errorf("re-add should not error: %v", err)
	}
	a, _ := s.GetAgent(ctx, "bosun")
	if len(a.Aliases) != 1 {
		t.Errorf("alias count = %d, want 1 after dedup", len(a.Aliases))
	}
}

func TestSetAliases_AgentMissing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	err := s.SetAliases(ctx, "ghost", []string{"x"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestListAgents_IncludesAliases(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bosun", "%2")
	_ = s.AddAlias(ctx, "bosun", "MBoN")
	_ = s.UpsertAgent(ctx, "surveyor", "%3")

	all, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, a := range all {
		if a.Name == "bosun" {
			if len(a.Aliases) != 1 || a.Aliases[0] != "MBoN" {
				t.Errorf("bosun aliases = %v", a.Aliases)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("bosun not in list")
	}
}

func TestAddAlias_RejectsCrossCanonicalCollision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "admin", "%5")
	_ = s.UpsertAgent(ctx, "pilot", "%4")

	// pilot tries to claim "Alcatraz Infra Admin" — but that's a
	// fine non-collision starting point.
	if err := s.AddAlias(ctx, "admin", "Alcatraz Infra Admin"); err != nil {
		t.Fatalf("admin's own alias: %v", err)
	}

	// Now pilot tries to add "Alcatraz Infra Admin" as its alias.
	err := s.AddAlias(ctx, "pilot", "Alcatraz Infra Admin")
	if !errors.Is(err, ErrAliasCollision) {
		t.Errorf("want ErrAliasCollision, got %v", err)
	}

	// And pilot tries to use admin's canonical name as an alias.
	err = s.AddAlias(ctx, "pilot", "admin")
	if !errors.Is(err, ErrAliasCollision) {
		t.Errorf("want ErrAliasCollision (canonical name as alias), got %v", err)
	}
}

func TestAddAlias_SelfRebindIsFine(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "admin", "%5")
	if err := s.AddAlias(ctx, "admin", "Admin"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	// Re-adding the same alias is idempotent (not a collision).
	if err := s.AddAlias(ctx, "admin", "Admin"); err != nil {
		t.Errorf("re-adding own alias should not collide: %v", err)
	}
}

// TestUpsertAgent_RejectsReservedRoutingName pins the substrate-honest
// rejection of routing primitives at registration time (Surveyor N3 on
// PR #257). A chamber registering as "operator" would shadow the
// send-side resolver — the resolver matches against the literal name
// before resolving, so the phantom registration would harvest
// operator-directed traffic.
func TestUpsertAgent_RejectsReservedRoutingName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	err := s.UpsertAgent(ctx, "operator", "%9")
	if !errors.Is(err, ErrReservedRoutingName) {
		t.Errorf("want ErrReservedRoutingName, got %v", err)
	}
}

// TestAddAlias_RejectsReservedRoutingName pins the same reservation on
// the alias path: a real chamber claiming "operator" as an alias would
// shadow the same routing primitive (Surveyor N3 on PR #257).
func TestAddAlias_RejectsReservedRoutingName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%5")
	err := s.AddAlias(ctx, "alice", "operator")
	if !errors.Is(err, ErrReservedRoutingName) {
		t.Errorf("want ErrReservedRoutingName, got %v", err)
	}
}
