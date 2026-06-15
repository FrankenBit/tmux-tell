package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

func TestInbox_TextFormat(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello bob",
	})

	var stdout, stderr bytes.Buffer
	exit := runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, false, "text", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ID\tFROM\tTO") {
		t.Errorf("missing header in %q", out)
	}
	if !strings.Contains(out, "alice\tbob") {
		t.Errorf("missing data row in %q", out)
	}
}

// TestInbox_BacklogFencedAnnotation pins Fix A of #390: queued rows below the
// agent's backlog floor (and not promoted-deferred) render `queued
// (backlog-fenced)` in text + carry `backlog_fenced: true` on the stable JSON
// surface, while above-floor rows and promoted-deferred rows do not.
func TestInbox_BacklogFencedAnnotation(t *testing.T) {
	s := newCmdTestStore(t, "bosun", "lookout")
	ctx := context.Background()
	// m1,m2,m3: normal queued (below the floor we set). promoted: queued WITH
	// deliver_after (at/below floor but must NOT fence). m4: normal queued above
	// the floor (must NOT fence).
	for _, b := range []string{"m1", "m2", "m3"} {
		_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "bosun", ToAgent: "lookout", Body: b})
	}
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "bosun", ToAgent: "lookout", Body: "promoted", DeliverAfter: "resume"})
	if _, err := s.PromoteDeferred(ctx, "lookout", "resume"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "bosun", ToAgent: "lookout", Body: "m4"})

	all, err := s.ListMessages(ctx, store.ListFilter{ToAgent: "lookout", State: store.StateQueued})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Floor at the promoted-deferred row's id: m1,m2,m3 are below it (→ fenced),
	// the promoted row is AT it but deliver_after-excluded, m4 is above it.
	var floor int64
	for _, m := range all {
		if m.DeliverAfter.Valid {
			floor = m.ID
		}
	}
	if floor == 0 {
		t.Fatal("could not locate promoted-deferred row id")
	}
	if err := s.SetBacklogEpoch(ctx, "lookout", floor); err != nil {
		t.Fatalf("set epoch: %v", err)
	}

	// Text: exactly 3 fenced rows (m1,m2,m3).
	var stdout bytes.Buffer
	if exit := runInboxWithStore(ctx, s, "lookout", store.StateQueued, 100, false, "text", &stdout, &bytes.Buffer{}); exit != exitOK {
		t.Fatalf("text exit = %d", exit)
	}
	if got := strings.Count(stdout.String(), "queued (backlog-fenced)"); got != 3 {
		t.Errorf("text backlog-fenced count = %d, want 3\n%s", got, stdout.String())
	}

	// JSON: every row carries backlog_fenced (stable surface); exactly 3 true.
	var jb bytes.Buffer
	if exit := runInboxWithStore(ctx, s, "lookout", store.StateQueued, 100, false, "json", &jb, &bytes.Buffer{}); exit != exitOK {
		t.Fatalf("json exit = %d", exit)
	}
	var rows []map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(jb.Bytes()), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	trueCount := 0
	for _, r := range rows {
		v, ok := r["backlog_fenced"]
		if !ok {
			t.Errorf("row %v missing backlog_fenced (stable surface must always emit it)", r["id"])
		}
		if v == true {
			trueCount++
		}
	}
	if trueCount != 3 {
		t.Errorf("json backlog_fenced=true count = %d, want 3", trueCount)
	}
}

func TestInbox_JSONFormat(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, _ := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello bob",
	})

	var stdout, stderr bytes.Buffer
	exit := runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, false, "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["id"] != res.PublicID {
		t.Errorf("id = %v, want %s", rows[0]["id"], res.PublicID)
	}
	if rows[0]["from"] != "alice" || rows[0]["to"] != "bob" {
		t.Errorf("from/to = %v/%v, want alice/bob", rows[0]["from"], rows[0]["to"])
	}
}

func TestInbox_FilterByState(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "2"})
	_, _ = s.ClaimNext(ctx, "bob")

	var stdout bytes.Buffer
	exit := runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, false, "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 1 {
		t.Errorf("queued rows = %d, want 1", len(rows))
	}

	stdout.Reset()
	exit = runInboxWithStore(ctx, s, "bob", store.StateDelivering, 100, false, "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 1 {
		t.Errorf("delivering rows = %d, want 1", len(rows))
	}
}

func TestInbox_EmptyTable(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	var stdout, stderr bytes.Buffer
	exit := runInboxWithStore(context.Background(), s, "bob", store.StateQueued, 100, false, "text", &stdout, &stderr)
	if exit != exitOK {
		t.Errorf("exit = %d, want 0", exit)
	}
	if !strings.Contains(stdout.String(), "ID\tFROM\tTO") {
		t.Errorf("should still print header, got %q", stdout.String())
	}
}

func TestInbox_UnknownFormat(t *testing.T) {
	s := newCmdTestStore(t, "bob")
	var stdout, stderr bytes.Buffer
	exit := runInboxWithStore(context.Background(), s, "bob", store.StateQueued, 100, false, "xml", &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d", exit, exitUsage)
	}
}

// --- #221 ack tests ---

// tfSetBacklogEpoch stamps the backlog epoch directly on an agent row.
// Used in tests to simulate what the register flow does.
func tfSetBacklogEpoch(t *testing.T, s *store.Store, agent string) {
	t.Helper()
	ctx := context.Background()
	// Highest queued id addressed to this agent becomes the epoch.
	msgs, err := s.ListMessages(ctx, store.ListFilter{ToAgent: agent, State: store.StateQueued, Limit: 1000, OrderDesc: true})
	if err != nil || len(msgs) == 0 {
		t.Fatalf("tfSetBacklogEpoch: no queued msgs for %s (err=%v)", agent, err)
	}
	// GetMessage to get the internal id.
	m, err := s.GetMessage(ctx, msgs[0].PublicID)
	if err != nil {
		t.Fatalf("tfSetBacklogEpoch: get msg: %v", err)
	}
	if err := s.SetBacklogEpoch(ctx, agent, m.ID); err != nil {
		t.Fatalf("tfSetBacklogEpoch: set epoch: %v", err)
	}
}

func TestInbox_AckSingle(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hello"})

	var stdout, stderr bytes.Buffer
	exit := runInboxAck(ctx, s, "bob", res.PublicID, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if got["id"] != res.PublicID {
		t.Errorf("id = %v, want %s", got["id"], res.PublicID)
	}

	// Message must no longer appear in the default queued view.
	var out bytes.Buffer
	runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, false, "json", &out, &bytes.Buffer{})
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(out.Bytes()), &rows)
	if len(rows) != 0 {
		t.Errorf("queued after ack = %d, want 0", len(rows))
	}
}

func TestInbox_AckAll_RoundTrip(t *testing.T) {
	// Full round-trip: seed backlog, stamp epoch, --ack-all, verify inbox clean + get works.
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	var backlogIDs []string
	for i := 0; i < 3; i++ {
		res, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "old"})
		backlogIDs = append(backlogIDs, res.PublicID)
	}
	// Stamp backlog epoch (simulates what register does).
	tfSetBacklogEpoch(t, s, "bob")

	// Insert a new arrival AFTER the epoch stamp — must survive ack-all.
	newRes, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "new"})

	var stdout, stderr bytes.Buffer
	exit := runInboxAckAll(ctx, s, "bob", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true {
		t.Errorf("ok = %v, want true", got["ok"])
	}
	if ackedN, _ := got["acked"].(float64); int(ackedN) != 3 {
		t.Errorf("acked = %v, want 3", got["acked"])
	}

	// Default inbox (queued) must show only the new arrival.
	var out bytes.Buffer
	runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, false, "json", &out, &bytes.Buffer{})
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(out.Bytes()), &rows)
	if len(rows) != 1 || rows[0]["id"] != newRes.PublicID {
		t.Errorf("queued after ack-all = %v, want only [%s]", rows, newRes.PublicID)
	}

	// get must still retrieve acknowledged backlog messages.
	for _, id := range backlogIDs {
		m, err := s.GetMessage(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if m.State != store.StateAcknowledged {
			t.Errorf("msg %s state = %s, want acknowledged", id, m.State)
		}
	}
}

func TestInbox_AckSingle_Idempotent(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "x"})

	var stdout, stderr bytes.Buffer
	if exit := runInboxAck(ctx, s, "bob", res.PublicID, &stdout, &stderr); exit != exitOK {
		t.Fatalf("first ack exit = %d; stderr=%s", exit, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	// Second call must succeed (idempotent).
	if exit := runInboxAck(ctx, s, "bob", res.PublicID, &stdout, &stderr); exit != exitOK {
		t.Errorf("second ack exit = %d (want 0); stderr=%s", exit, stderr.String())
	}
}

func TestInbox_AckSingle_AuthScope(t *testing.T) {
	// carol cannot ack bob's message.
	s := newCmdTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()
	res, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "y"})

	var stdout, stderr bytes.Buffer
	exit := runInboxAck(ctx, s, "carol", res.PublicID, &stdout, &stderr)
	if exit == exitOK {
		t.Errorf("carol acking bob's message should fail, got exitOK")
	}
}

func TestInbox_DefaultExcludesAcknowledged(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	res, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "z"})

	// Ack it.
	_ = s.MarkAcknowledged(ctx, "bob", res.PublicID)

	// Default inbox (queued) must be empty.
	var stdout bytes.Buffer
	exit := runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, false, "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 0 {
		t.Errorf("queued inbox includes acknowledged message, want 0 rows; got %v", rows)
	}
}

// TestInbox_Unanswered: --unanswered returns only expects_reply=1 messages
// that bob has not replied to, and the expects_reply field is set in JSON output.
func TestInbox_Unanswered(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	// Alice sends two asks to bob.
	ask1, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "q1?", ExpectsReply: true})
	ask2, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "q2?", ExpectsReply: true})
	// Plain send — must not appear in --unanswered output.
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "fyi"})

	// Bob replies to ask1.
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "bob", ToAgent: "alice", ReplyTo: ask1.PublicID, Body: "a1"})

	var stdout bytes.Buffer
	exit := runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, true, "json", &stdout, &bytes.Buffer{})
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	var rows []map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rows)
	if len(rows) != 1 {
		t.Fatalf("unanswered = %d rows, want 1", len(rows))
	}
	if rows[0]["id"] != ask2.PublicID {
		t.Errorf("id = %v, want %s", rows[0]["id"], ask2.PublicID)
	}
	// JSON output must carry expects_reply=true.
	if rows[0]["expects_reply"] != true {
		t.Errorf("expects_reply = %v, want true", rows[0]["expects_reply"])
	}
}
