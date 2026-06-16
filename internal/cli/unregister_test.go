package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestMCP_Unregister_FullLifecycle is the acceptance-criteria test for #289:
// register agents, send a queued message, unregister with force+purge_queue,
// verify agent row and queued message are gone.
func TestMCP_Unregister_FullLifecycle(t *testing.T) {
	fs := &fakeSystemctl{}
	fs.install(t)

	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	// Seed a queued message from alice → bob.
	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello",
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	// Unregister bob with force + purge_queue.
	got := callMCPTool(t, s, "tmux-tell.unregister", map[string]any{
		"name":        "bob",
		"purge_queue": true,
		"force":       true,
	})
	if got["ok"] != true {
		t.Fatalf("ok = %v; got=%v", got["ok"], got)
	}
	if got["removed"] != true {
		t.Errorf("removed = %v, want true", got["removed"])
	}
	if got["mailman"] != "stopped" {
		t.Errorf("mailman = %v, want stopped", got["mailman"])
	}
	deleted, _ := got["deleted"].(float64)
	if int(deleted) != 1 {
		t.Errorf("deleted = %v, want 1", deleted)
	}

	// Agent row must be gone.
	_, err := s.GetAgent(ctx, "bob")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAgent(bob) = %v, want ErrNotFound", err)
	}

	// Queued message must be gone (purge_queue was set).
	depth, err := s.RecipientQueueDepth(ctx, "bob")
	if err != nil {
		t.Fatalf("RecipientQueueDepth: %v", err)
	}
	if depth != 0 {
		t.Errorf("queue depth after purge = %d, want 0", depth)
	}
}

// TestMCP_Unregister_DefaultPreservesQueue verifies the default behavior:
// without purge_queue, queued messages stay in the table so the sender's
// sent history is intact and messages can deliver if the agent re-registers.
func TestMCP_Unregister_DefaultPreservesQueue(t *testing.T) {
	fs := &fakeSystemctl{}
	fs.install(t)

	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	msg, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "will survive",
	})
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	// Unregister without purge_queue — force is needed because there is a queued message.
	got := callMCPTool(t, s, "tmux-tell.unregister", map[string]any{
		"name":  "bob",
		"force": true,
	})
	if got["ok"] != true {
		t.Fatalf("ok = %v; got=%v", got["ok"], got)
	}
	deleted, _ := got["deleted"].(float64)
	if int(deleted) != 0 {
		t.Errorf("deleted = %v, want 0 (purge_queue not set)", deleted)
	}

	// Message survives: alice's sent history shows it.
	msgs, err := s.ListMessages(ctx, store.ListFilter{
		FromAgent: "alice", Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	found := false
	for _, m := range msgs {
		if m.PublicID == msg.PublicID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sender's message %s missing from sent history after unregister without purge_queue", msg.PublicID)
	}
}

// TestMCP_Unregister_Idempotent verifies that unregistering an absent agent
// returns ok:true with removed:false rather than an error.
func TestMCP_Unregister_Idempotent(t *testing.T) {
	fs := &fakeSystemctl{}
	fs.install(t)

	s := newCmdTestStore(t)
	got := callMCPTool(t, s, "tmux-tell.unregister", map[string]any{
		"name": "ghost",
	})
	if got["ok"] != true {
		t.Fatalf("ok = %v; got=%v", got["ok"], got)
	}
	if got["removed"] != false {
		t.Errorf("removed = %v, want false", got["removed"])
	}
}

// TestMCP_Unregister_QueueGuard verifies that unregistering an agent with
// queued messages fails unless force:true is set.
func TestMCP_Unregister_QueueGuard(t *testing.T) {
	fs := &fakeSystemctl{}
	fs.install(t)

	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "pending",
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	// Without force — should fail.
	got := callMCPTool(t, s, "tmux-tell.unregister", map[string]any{
		"name": "bob",
	})
	if got["ok"] == true {
		t.Errorf("expected failure when bob has queued messages; got ok=true")
	}
	errStr, _ := got["error"].(string)
	if errStr == "" {
		errStr, _ = got["message"].(string)
	}
	if errStr == "" {
		t.Logf("guard response: %v", got)
	}

	// Bob's row must still exist.
	_, err := s.GetAgent(ctx, "bob")
	if err != nil {
		t.Errorf("GetAgent(bob) after guard refusal = %v, want nil", err)
	}

	// With force — should succeed.
	got = callMCPTool(t, s, "tmux-tell.unregister", map[string]any{
		"name":  "bob",
		"force": true,
	})
	if got["ok"] != true {
		t.Errorf("expected ok=true with force; got=%v", got)
	}
}

// TestMCP_Unregister_PreservesHistory verifies that history messages
// (delivered/failed) are NOT deleted by purge_queue (which only purges queued).
func TestMCP_Unregister_PreservesHistory(t *testing.T) {
	fs := &fakeSystemctl{}
	fs.install(t)

	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	// Insert a queued message.
	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "queued",
	}); err != nil {
		t.Fatalf("insert queued: %v", err)
	}

	// Claim + mark delivered so it transitions to StateDelivered.
	claimed, err := s.ClaimNext(ctx, "bob")
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if err := s.MarkDelivered(ctx, claimed.PublicID); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	// Unregister with purge_queue.
	got := callMCPTool(t, s, "tmux-tell.unregister", map[string]any{
		"name":        "bob",
		"purge_queue": true,
		"force":       true,
	})
	if got["ok"] != true {
		t.Fatalf("ok = %v; got=%v", got["ok"], got)
	}
	// purge_queue only removes StateQueued — there were 0 queued messages
	// (we delivered the only one), so deleted = 0.
	deleted, _ := got["deleted"].(float64)
	if int(deleted) != 0 {
		t.Errorf("deleted = %v, want 0 (no queued messages; delivered row preserved)", deleted)
	}

	// The delivered message must still be in the DB.
	msgs, err := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "bob", State: store.StateDelivered, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("delivered messages = %d, want 1 (history preserved)", len(msgs))
	}
}

// TestCLI_Unregister_FullLifecycle exercises runUnregisterCLI via a temp-file
// DB (the in-memory newCmdTestStore can't be shared with the CLI's own
// store.Open).
func TestCLI_Unregister_FullLifecycle(t *testing.T) {
	fs := &fakeSystemctl{}
	fs.install(t)

	dbPath := filepath.Join(t.TempDir(), "messages.db")
	ctx := context.Background()

	seed, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	for _, n := range []string{"alice", "bob"} {
		if err := seed.UpsertAgent(ctx, n, "%99"); err != nil {
			t.Fatalf("seed agent %s: %v", n, err)
		}
	}
	if _, err := seed.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hi",
	}); err != nil {
		t.Fatalf("seed msg: %v", err)
	}
	_ = seed.Close()

	t.Setenv("CLAUDE_MSG_DB", dbPath)
	var stdout, stderr bytes.Buffer
	exit := runUnregisterCLI([]string{
		"--name", "bob", "--force", "--purge-queue",
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want exitOK; stderr=%s stdout=%s", exit, stderr.String(), stdout.String())
	}
	out := parseJSONResult(t, stdout.Bytes())
	if out["ok"] != true {
		t.Errorf("ok = %v, want true", out["ok"])
	}
	if out["removed"] != true {
		t.Errorf("removed = %v, want true", out["removed"])
	}

	// Verify via a fresh store open.
	verify, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open verify: %v", err)
	}
	defer verify.Close()

	_, err = verify.GetAgent(ctx, "bob")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAgent(bob) = %v, want ErrNotFound", err)
	}
	depth, err := verify.RecipientQueueDepth(ctx, "bob")
	if err != nil {
		t.Fatalf("RecipientQueueDepth: %v", err)
	}
	if depth != 0 {
		t.Errorf("queue depth = %d, want 0", depth)
	}
}

// TestCLI_Unregister_Idempotent verifies CLI idempotency: absent agent → ok, removed:false.
func TestCLI_Unregister_Idempotent(t *testing.T) {
	fs := &fakeSystemctl{}
	fs.install(t)

	dbPath := filepath.Join(t.TempDir(), "messages.db")
	seed, _ := store.Open(dbPath)
	_ = seed.Close()

	t.Setenv("CLAUDE_MSG_DB", dbPath)
	var stdout, stderr bytes.Buffer
	exit := runUnregisterCLI([]string{"--name", "ghost"}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want exitOK; stderr=%s", exit, stderr.String())
	}
	out := parseJSONResult(t, stdout.Bytes())
	if out["removed"] != false {
		t.Errorf("removed = %v, want false", out["removed"])
	}
}

// TestCLI_Unregister_SoftFailsSystemctlError pins #338's substrate-honest
// framing: when `systemctl --user disable --now` errors with anything other
// than the idempotent not-loaded shape, the unregister continues — DB row
// removal succeeds, and the systemctl failure surfaces as `mailman: "warn"`
// + `mailman_error`. The alternative (hard-fail like pre-#338) would have
// left the agent row stranded if the user systemd manager flaked, which is
// the opposite of substrate-honest cleanup.
func TestCLI_Unregister_SoftFailsSystemctlError(t *testing.T) {
	fs := &fakeSystemctl{err: errors.New("Failed to connect to user bus")}
	fs.install(t)

	dbPath := filepath.Join(t.TempDir(), "messages.db")
	ctx := context.Background()
	seed, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if err := seed.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	_ = seed.Close()

	t.Setenv("CLAUDE_MSG_DB", dbPath)
	var stdout, stderr bytes.Buffer
	exit := runUnregisterCLI([]string{"--name", "bob"}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want exitOK (soft-fail); stderr=%s stdout=%s",
			exit, stderr.String(), stdout.String())
	}
	out := parseJSONResult(t, stdout.Bytes())
	if out["ok"] != true {
		t.Errorf("ok = %v, want true", out["ok"])
	}
	if out["removed"] != true {
		t.Errorf("removed = %v, want true (row removal proceeds despite systemctl error)",
			out["removed"])
	}
	if out["mailman"] != "warn" {
		t.Errorf("mailman = %v, want \"warn\"", out["mailman"])
	}
	mmErr, _ := out["mailman_error"].(string)
	if mmErr == "" {
		t.Errorf("mailman_error empty; want systemctl error surface")
	}

	// Authoritative state check: the DB row is gone.
	verify, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open verify: %v", err)
	}
	defer verify.Close()
	if _, err := verify.GetAgent(ctx, "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAgent(bob) = %v, want ErrNotFound", err)
	}
}

// TestMCP_Unregister_SoftFailsSystemctlError mirrors the CLI soft-fail check
// on the MCP surface so both entry points share the substrate-honest shape.
func TestMCP_Unregister_SoftFailsSystemctlError(t *testing.T) {
	fs := &fakeSystemctl{err: errors.New("Failed to connect to user bus")}
	fs.install(t)

	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	got := callMCPTool(t, s, "tmux-tell.unregister", map[string]any{
		"name": "bob",
	})
	if got["ok"] != true {
		t.Fatalf("ok = %v; got=%v", got["ok"], got)
	}
	if got["removed"] != true {
		t.Errorf("removed = %v, want true", got["removed"])
	}
	if got["mailman"] != "warn" {
		t.Errorf("mailman = %v, want \"warn\"", got["mailman"])
	}
	mmErr, _ := got["mailman_error"].(string)
	if mmErr == "" {
		t.Errorf("mailman_error empty; want systemctl error surface")
	}

	if _, err := s.GetAgent(ctx, "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetAgent(bob) = %v, want ErrNotFound", err)
	}
}
