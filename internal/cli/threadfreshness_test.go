package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// tfInsert inserts a message (optionally threaded under replyTo) and returns
// its public_id. Rows are inserted in call order, so their rowids are strictly
// increasing — the arrival order resolveThreadFreshness reasons over.
func tfInsert(t *testing.T, s *store.Store, from, to, replyTo string) string {
	t.Helper()
	r, err := s.InsertMessage(context.Background(), store.InsertParams{
		FromAgent: from, ToAgent: to, ReplyTo: replyTo, Body: "x",
	})
	if err != nil {
		t.Fatalf("insert %s→%s replyTo=%q: %v", from, to, replyTo, err)
	}
	return r.PublicID
}

// --- unit tests on the resolver (the substrate-knowable definition) ---

func TestThreadFreshness_NoNewer(t *testing.T) {
	// Thread is just alice's own message; she replies to it. Nothing arrived
	// addressed to her since she last spoke → not stale.
	s := newCmdTestStore(t, "alice", "bob")
	m1 := tfInsert(t, s, "alice", "bob", "")

	tf, err := resolveThreadFreshness(context.Background(), s, m1, "alice")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tf.Stale || len(tf.NewerInThread) != 0 {
		t.Errorf("freshness = %+v, want not-stale + no newer", tf)
	}
	if tf.YouRepliedTo != m1 || tf.LatestInThread != m1 {
		t.Errorf("freshness = %+v, want you_replied_to=latest=%s", tf, m1)
	}
}

func TestThreadFreshness_NewerToOthersOnly(t *testing.T) {
	// A newer message exists in the thread, but it's addressed to charlie,
	// not the sender (alice) — not alice's concern → not stale.
	s := newCmdTestStore(t, "alice", "bob", "charlie")
	m1 := tfInsert(t, s, "alice", "bob", "")
	_ = tfInsert(t, s, "bob", "charlie", m1) // m2: to charlie

	tf, err := resolveThreadFreshness(context.Background(), s, m1, "alice")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tf.Stale || len(tf.NewerInThread) != 0 {
		t.Errorf("freshness = %+v, want not-stale (newer is to charlie)", tf)
	}
}

func TestThreadFreshness_NewerToSenderAfterTheirLast(t *testing.T) {
	// The crossed case: alice spoke (m1), bob replied to alice (m2) before
	// alice's next send. m2 is addressed to alice and arrived after her last
	// message → stale, flagged.
	s := newCmdTestStore(t, "alice", "bob")
	m1 := tfInsert(t, s, "alice", "bob", "")
	m2 := tfInsert(t, s, "bob", "alice", m1)

	tf, err := resolveThreadFreshness(context.Background(), s, m1, "alice")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !tf.Stale || len(tf.NewerInThread) != 1 || tf.NewerInThread[0] != m2 {
		t.Errorf("freshness = %+v, want stale + newer=[%s]", tf, m2)
	}
	if tf.LatestInThread != m2 {
		t.Errorf("latest_in_thread = %q, want %q", tf.LatestInThread, m2)
	}
}

func TestThreadFreshness_ReplyToLatest_NotStale(t *testing.T) {
	// The common case + Surveyor's #189 false-positive guard: alice spoke (m1),
	// bob replied to her (m2). alice replies to m2 — the LATEST message, which
	// she's demonstrably holding. The reply_to target folds into the baseline,
	// so m2 must NOT count as newer. (Pre-fix this reported stale on every
	// normal reply-to-the-latest.)
	s := newCmdTestStore(t, "alice", "bob")
	m1 := tfInsert(t, s, "alice", "bob", "")
	m2 := tfInsert(t, s, "bob", "alice", m1)

	tf, err := resolveThreadFreshness(context.Background(), s, m2, "alice")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tf.Stale || len(tf.NewerInThread) != 0 {
		t.Errorf("freshness = %+v, want not-stale (replying to the latest)", tf)
	}
}

func TestThreadFreshness_ReplyToIntermediate_NotStale(t *testing.T) {
	// alice replies to m3 (the latest) while an earlier message to her (m2)
	// also exists. Anchoring to the reply_to target as high-water-mark means m2
	// — older than what she's holding — is not flagged: she's replying to the
	// newest, so nothing is "newer than" her anchor.
	s := newCmdTestStore(t, "alice", "bob")
	m1 := tfInsert(t, s, "alice", "bob", "")
	m2 := tfInsert(t, s, "bob", "alice", m1)
	m3 := tfInsert(t, s, "bob", "alice", m2)

	tf, err := resolveThreadFreshness(context.Background(), s, m3, "alice")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tf.Stale || len(tf.NewerInThread) != 0 {
		t.Errorf("freshness = %+v, want not-stale (replying to latest m3)", tf)
	}
}

func TestThreadFreshness_SenderHasntSpoken_AnchorsToReplyToTarget(t *testing.T) {
	// alice enters a thread cold (never spoke in it). Baseline falls back to
	// the reply_to target's arrival point; a later message addressed to her
	// (m3) counts as newer.
	s := newCmdTestStore(t, "alice", "bob", "charlie")
	m1 := tfInsert(t, s, "bob", "charlie", "")
	m2 := tfInsert(t, s, "charlie", "bob", m1)
	m3 := tfInsert(t, s, "bob", "alice", m2) // addressed to alice, after m1

	tf, err := resolveThreadFreshness(context.Background(), s, m1, "alice")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !tf.Stale || len(tf.NewerInThread) != 1 || tf.NewerInThread[0] != m3 {
		t.Errorf("freshness = %+v, want stale + newer=[%s]", tf, m3)
	}
}

func TestThreadFreshness_UnknownReplyTo(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	_, err := resolveThreadFreshness(context.Background(), s, "nope", "alice")
	if err == nil {
		t.Fatal("resolve(unknown reply_to) = nil error, want ErrNotFound")
	}
}

// --- end-to-end tests through the CLI send path ---

func TestSend_AbsentReplyTo_NoFreshnessCheck(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	withReachability(t, map[string]bool{"%3": true}, true)

	var stdout, stderr bytes.Buffer
	// No ReplyTo → no thread_freshness block at all.
	if exit := runSendWithStore(ctx, s, baseSendParams("alice", "bob"), &stdout, &stderr); exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if r.Freshness != nil {
		t.Errorf("freshness = %+v, want nil (no reply_to)", r.Freshness)
	}
	// And the JSON omits the key entirely (omitempty).
	if strings.Contains(stdout.String(), "thread_freshness") {
		t.Errorf("output contains thread_freshness key without reply_to:\n%s", stdout.String())
	}
}

// staleThreadStore seeds a crossed thread: alice→bob (m1), then bob→alice (m2)
// replying to m1. With alice as sender replying to m1, the thread is stale (m2
// addressed to her arrived after her last message). Returns the store + m1 + m2.
// Each call is an independent store so a send in one scenario doesn't move
// alice's baseline for the next.
func staleThreadStore(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	m1 := tfInsert(t, s, "alice", "bob", "")
	m2 := tfInsert(t, s, "bob", "alice", m1)
	return s, m1, m2
}

func TestSend_ReplyToStale_DefaultQueuesReportsStale(t *testing.T) {
	s, m1, m2 := staleThreadStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)

	var stdout, stderr bytes.Buffer
	p := baseSendParams("alice", "bob")
	p.ReplyTo = m1
	if exit := runSendWithStore(context.Background(), s, p, &stdout, &stderr); exit != exitOK {
		t.Fatalf("default stale exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if !r.OK || r.Freshness == nil || !r.Freshness.Stale {
		t.Fatalf("default stale = %+v / freshness %+v, want ok + stale", r, r.Freshness)
	}
	if len(r.Freshness.NewerInThread) != 1 || r.Freshness.NewerInThread[0] != m2 {
		t.Errorf("newer_in_thread = %v, want [%s]", r.Freshness.NewerInThread, m2)
	}
}

func TestSend_ReplyToStale_BlockOnStaleRefuses(t *testing.T) {
	s, m1, _ := staleThreadStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)

	var stdout, stderr bytes.Buffer
	p := baseSendParams("alice", "bob")
	p.ReplyTo = m1
	p.BlockOnStale = true
	if exit := runSendWithStore(context.Background(), s, p, &stdout, &stderr); exit != exitUnavailable {
		t.Errorf("block-on-stale exit = %d, want exitUnavailable", exit)
	}
	r := decodeSend(t, stdout.Bytes())
	if r.OK || r.Freshness == nil || r.Error == "" {
		t.Errorf("block-on-stale = %+v, want ok:false + freshness + error", r)
	}
}

func TestSend_ReplyToFresh_BlockOnStalePasses(t *testing.T) {
	// --block-on-stale set, but the thread hasn't moved → send succeeds and
	// carries a not-stale freshness block.
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	withReachability(t, map[string]bool{"%3": true}, true)

	m1 := tfInsert(t, s, "alice", "bob", "")

	var stdout, stderr bytes.Buffer
	p := baseSendParams("alice", "bob")
	p.ReplyTo = m1
	p.BlockOnStale = true
	if exit := runSendWithStore(ctx, s, p, &stdout, &stderr); exit != exitOK {
		t.Fatalf("fresh block-on-stale exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if !r.OK || r.Freshness == nil || r.Freshness.Stale {
		t.Errorf("fresh = %+v / freshness %+v, want ok + not-stale", r, r.Freshness)
	}
}

func TestSend_TextFormat_StaleWarning(t *testing.T) {
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	withReachability(t, map[string]bool{"%3": true}, true)

	m1 := tfInsert(t, s, "alice", "bob", "")
	_ = tfInsert(t, s, "bob", "alice", m1)

	p := baseSendParams("alice", "bob")
	p.ReplyTo = m1
	p.Format = "text"
	var stdout, stderr bytes.Buffer
	if exit := runSendWithStore(ctx, s, p, &stdout, &stderr); exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	out := stdout.String()
	for _, want := range []string{"newer message(s) in this thread", "latest in thread:"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q; got:\n%s", want, out)
		}
	}
}
