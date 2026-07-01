package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// callMCPTool invokes one tool against the test server and returns the
// parsed tool result.
func callMCPTool(t *testing.T, s *store.Store, name string, args map[string]any) map[string]any {
	t.Helper()
	srv := newMCPServer(s)

	argsBytes, _ := json.Marshal(args)
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": json.RawMessage(argsBytes),
		},
	}
	reqLine, _ := json.Marshal(req)

	in := bytes.NewReader(append(reqLine, '\n'))
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), in, &out); err != nil && err != io.EOF {
		t.Fatalf("Serve: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v; out=%s", err, out.String())
	}
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %d; result=%v", len(content), result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	isErr := result["isError"] == true
	// Tools can return JSON objects, JSON arrays, or (on error) plain
	// text. Detect each.
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		if isErr {
			payload["_isError"] = true
		}
		return payload
	}
	var arr any
	if err := json.Unmarshal([]byte(text), &arr); err == nil {
		return map[string]any{"_array": arr, "_isError": isErr}
	}
	// Plain text — typically an error message.
	return map[string]any{"_text": text, "_isError": isErr}
}

func TestMCP_Send_HappyPath(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")

	got := callMCPTool(t, s, "tmux-tell.send", map[string]any{
		"to":   "bob",
		"body": "hello via mcp",
	})
	if got["ok"] != true {
		t.Errorf("ok = %v; got=%v", got["ok"], got)
	}
	id, _ := got["id"].(string)
	if len(id) != 4 {
		t.Errorf("id = %q, want 4 hex chars", id)
	}
	receipt, ok := got["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt missing or wrong type; got=%v", got["receipt"])
	}
	enqueue, ok := receipt["enqueue"].(map[string]any)
	if !ok || enqueue["state"] != "accepted" || enqueue["at"] == "" {
		t.Errorf("enqueue receipt = %v, want accepted with timestamp", receipt["enqueue"])
	}
	dispatch, ok := receipt["dispatch"].(map[string]any)
	if !ok || dispatch["state"] != "not_requested" {
		t.Errorf("dispatch receipt = %v, want not_requested", receipt["dispatch"])
	}
	paste, ok := receipt["paste_confirmed"].(map[string]any)
	if !ok || paste["state"] != "not_requested" {
		t.Errorf("paste receipt = %v, want not_requested", receipt["paste_confirmed"])
	}
}

func TestMCP_Send_UnknownRecipient(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice")

	got := callMCPTool(t, s, "tmux-tell.send", map[string]any{
		"to":   "ghost",
		"body": "hi",
	})
	if got["_isError"] != true {
		t.Errorf("isError should be true; got=%v", got)
	}
}

func TestMCP_Send_BlockOnStale(t *testing.T) {
	// MCP-path parity for #155: a stale thread + block_on_stale=true returns a
	// structured ok:false result (not an MCP error) carrying the freshness block.
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")
	m1 := tfInsert(t, s, "alice", "bob", "")
	_ = tfInsert(t, s, "bob", "alice", m1) // crosses in, addressed to alice

	got := callMCPTool(t, s, "tmux-tell.send", map[string]any{
		"to":             "bob",
		"body":           "late reply",
		"reply_to":       m1,
		"block_on_stale": true,
	})
	if got["_isError"] == true {
		t.Fatalf("block_on_stale should be a structured result, not an MCP error; got=%v", got)
	}
	if got["ok"] != false {
		t.Errorf("ok = %v, want false; got=%v", got["ok"], got)
	}
	tf, _ := got["thread_freshness"].(map[string]any)
	if tf == nil || tf["stale"] != true {
		t.Errorf("thread_freshness = %v, want stale:true", got["thread_freshness"])
	}
	if got["error"] == nil || got["error"] == "" {
		t.Errorf("error empty; want a stale-rejection message; got=%v", got)
	}
}

func TestMCP_Resend_GuardAndForce(t *testing.T) {
	// MCP-path parity for #157 PR1: a delivered message is refused (structured
	// ok:false, not an MCP error) without force, and replays with force.
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")
	orig := seedResendMsg(t, s, "alice", "bob", "mcp replay body", store.StateDelivered)

	// Without force → structured refusal carrying the replay block.
	got := callMCPTool(t, s, "tmux-tell.resend", map[string]any{"id": orig})
	if got["_isError"] == true {
		t.Fatalf("guard refusal should be structured, not an MCP error; got=%v", got)
	}
	if got["ok"] != false {
		t.Errorf("ok = %v, want false; got=%v", got["ok"], got)
	}
	if rp, _ := got["replay"].(map[string]any); rp == nil || rp["original_id"] != orig {
		t.Errorf("replay block = %v, want original_id=%s", got["replay"], orig)
	}

	// With force → ok:true replay.
	got2 := callMCPTool(t, s, "tmux-tell.resend", map[string]any{"id": orig, "force": true})
	if got2["ok"] != true {
		t.Errorf("force resend ok = %v; got=%v", got2["ok"], got2)
	}
	if rp, _ := got2["replay"].(map[string]any); rp == nil || rp["forced"] != true {
		t.Errorf("replay block = %v, want forced=true", got2["replay"])
	}
}

func TestMCP_Resend_UnknownID(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")
	got := callMCPTool(t, s, "tmux-tell.resend", map[string]any{"id": "ghost"})
	if got["_isError"] != true {
		t.Errorf("unknown id should be an MCP error; got=%v", got)
	}
}

func TestMCP_Send_RequiresEnvVar(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "")
	s := newCmdTestStore(t, "bob")

	got := callMCPTool(t, s, "tmux-tell.send", map[string]any{
		"to":   "bob",
		"body": "x",
	})
	if got["_isError"] != true {
		t.Errorf("missing env should be error; got=%v", got)
	}
}

func TestMCP_Whoami(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice")

	got := callMCPTool(t, s, "tmux-tell.whoami", map[string]any{})
	if got["ok"] != true || got["name"] != "alice" {
		t.Errorf("got %v", got)
	}
}

func TestMCP_Agents(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob", "carol")

	got := callMCPTool(t, s, "tmux-tell.agents", map[string]any{})
	arr, ok := got["_array"].([]any)
	if !ok {
		t.Fatalf("expected array result, got %v", got)
	}
	if len(arr) != 3 {
		t.Errorf("agents = %d, want 3", len(arr))
	}
}

// TestParseMCPToField_* pins the two parse shapes and the rejection paths for
// the `to` field (#158, #220 S1 test-gap closure).

func TestParseMCPToField_MultiRecipient(t *testing.T) {
	raw := json.RawMessage(`["alice","bob"]`)
	got, err := parseMCPToField(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("got %v, want [alice bob]", got)
	}
}

func TestParseMCPToField_SingleRecipient(t *testing.T) {
	raw := json.RawMessage(`"alice"`)
	got, err := parseMCPToField(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "alice" {
		t.Errorf("got %v, want [alice]", got)
	}
}

func TestParseMCPToField_InvalidShape(t *testing.T) {
	for _, bad := range []json.RawMessage{
		json.RawMessage(`42`),
		json.RawMessage(`null`),
		json.RawMessage(`{}`),
		json.RawMessage(``),
	} {
		if _, err := parseMCPToField(bad); err == nil {
			t.Errorf("parseMCPToField(%s) should error, got nil", bad)
		}
	}
}

// TestMCP_Send_QuickNoReplyExpectedMultiRecipient exercises the 3-way combined
// path: quick + no_reply_expected + multi-recipient fan-out via the MCP surface
// (#220 S1 test-gap closure). One message per recipient is trivially within
// the per-(sender,recipient) capSenderBacklog=2 cap (#296).
func TestMCP_Send_QuickNoReplyExpectedMultiRecipient(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob", "carol")

	got := callMCPTool(t, s, "tmux-tell.send", map[string]any{
		"to":                []string{"bob", "carol"},
		"body":              "quick fyi",
		"quick":             true,
		"no_reply_expected": true,
	})
	if got["_isError"] == true {
		t.Fatalf("unexpected MCP error: %v", got)
	}
	if got["ok"] != true {
		t.Errorf("ok = %v, want true; got=%v", got["ok"], got)
	}
	// Multi-recipient response carries a "messages" array, one entry per recipient.
	msgs, ok := got["messages"].([]any)
	if !ok {
		t.Fatalf("messages field missing or wrong type; got=%v", got)
	}
	if len(msgs) != 2 {
		t.Errorf("messages = %d, want 2", len(msgs))
	}
	for _, raw := range msgs {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("message row wrong type: %T", raw)
		}
		receipt, ok := row["receipt"].(map[string]any)
		if !ok {
			t.Fatalf("message receipt missing: %v", row)
		}
		enqueue, ok := receipt["enqueue"].(map[string]any)
		if !ok || enqueue["state"] != "accepted" {
			t.Errorf("message enqueue receipt = %v, want accepted", receipt["enqueue"])
		}
	}
	// Verify quick + no_reply_expected survive the round-trip through the store.
	ctx := context.Background()
	for _, to := range []string{"bob", "carol"} {
		m, err := s.ClaimNext(ctx, to)
		if err != nil || m == nil {
			t.Fatalf("ClaimNext(%s): m=%v err=%v", to, m, err)
		}
		if !m.Quick {
			t.Errorf("recipient %s: Quick = false, want true", to)
		}
		if !m.NoReplyExpected {
			t.Errorf("recipient %s: NoReplyExpected = false, want true", to)
		}
	}
}

func TestMCP_Inbox(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "bob")
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "queued"})

	got := callMCPTool(t, s, "tmux-tell.inbox", map[string]any{})
	arr, ok := got["_array"].([]any)
	if !ok {
		t.Fatalf("expected array, got %v", got)
	}
	if len(arr) != 1 {
		t.Errorf("inbox = %d, want 1", len(arr))
	}
}

func TestMCP_Status(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	got := callMCPTool(t, s, "tmux-tell.status", map[string]any{})
	arr, ok := got["_array"].([]any)
	if !ok {
		t.Fatalf("expected array, got %v", got)
	}
	if len(arr) != 2 {
		t.Errorf("status rows = %d, want 2", len(arr))
	}
}

// TestMCP_ToolsListContract pins the full list of advertised tools so
// adding/removing one is intentional.
func TestMCP_ToolsListContract(t *testing.T) {
	s := newCmdTestStore(t)
	srv := newMCPServer(s)
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	_ = srv.Serve(context.Background(), in, &out)
	var resp map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp)
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	want := map[string]bool{
		"tmux-tell.send":                true,
		"tmux-tell.resend":              true,
		"tmux-tell.flush_deferred":      true,
		"tmux-tell.ask":                 true,
		"tmux-tell.wait_for_reply":      true,
		"tmux-tell.check_replies":       true,
		"tmux-tell.ping":                true,
		"tmux-tell.whoami_db":           true,
		"tmux-tell.agents":              true,
		"tmux-tell.whoami":              true,
		"tmux-tell.inbox":               true,
		"tmux-tell.status":              true,
		"tmux-tell.register":            true,
		"tmux-tell.unregister":          true,
		"tmux-tell.control":             true,
		"tmux-tell.message_status":      true,
		"tmux-tell.get":                 true,
		"tmux-tell.agent_state":         true,
		"tmux-tell.flag_operator":       true,
		"tmux-tell.clear_operator_flag": true,
		"tmux-tell.set_pane_name":       true,
		"tmux-tell.set_metabolism":      true,
		"tmux-tell.set_session_id":      true,
	}
	if len(tools) != len(want) {
		t.Errorf("tools = %d, want %d", len(tools), len(want))
	}
	for _, ti := range tools {
		name := ti.(map[string]any)["name"].(string)
		if !want[name] {
			t.Errorf("unexpected tool advertised: %s", name)
		}
		delete(want, name)
	}
	for missing := range want {
		t.Errorf("missing tool: %s", missing)
	}
}

// --- #221 MCP ack tests ---

func TestMCP_Inbox_AckAll(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "bob")
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	// Insert 2 backlog messages and stamp the epoch.
	var lastID int64
	for i := 0; i < 2; i++ {
		res, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "old"})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		m, err := s.GetMessage(ctx, res.PublicID)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		lastID = m.ID
	}
	if err := s.SetBacklogEpoch(ctx, "bob", lastID); err != nil {
		t.Fatalf("SetBacklogEpoch: %v", err)
	}

	// New arrival after the epoch — must survive ack_all.
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "new"})

	got := callMCPTool(t, s, "tmux-tell.inbox", map[string]any{"ack_all": true})
	if got["_isError"] == true {
		t.Fatalf("unexpected error: %v", got)
	}
	if got["ok"] != true {
		t.Errorf("ok = %v, want true; got=%v", got["ok"], got)
	}
	if acked, _ := got["acked"].(float64); int(acked) != 2 {
		t.Errorf("acked = %v, want 2", got["acked"])
	}

	// Default inbox (queued) must show only the new arrival.
	inbox := callMCPTool(t, s, "tmux-tell.inbox", map[string]any{})
	arr, _ := inbox["_array"].([]any)
	if len(arr) != 1 {
		t.Errorf("queued after ack_all = %d, want 1", len(arr))
	}
}

func TestMCP_Inbox_AckIds(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "bob")
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()

	res1, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "m1"})
	res2, _ := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "m2"})

	got := callMCPTool(t, s, "tmux-tell.inbox", map[string]any{
		"ack_ids": []string{res1.PublicID},
	})
	if got["_isError"] == true {
		t.Fatalf("unexpected error: %v", got)
	}
	if got["ok"] != true {
		t.Errorf("ok = %v, want true; got=%v", got["ok"], got)
	}
	if acked, _ := got["acked"].(float64); int(acked) != 1 {
		t.Errorf("acked = %v, want 1", got["acked"])
	}

	// res2 must remain queued.
	inbox := callMCPTool(t, s, "tmux-tell.inbox", map[string]any{})
	arr, _ := inbox["_array"].([]any)
	if len(arr) != 1 {
		t.Fatalf("queued after ack_ids = %d, want 1", len(arr))
	}
	row, _ := arr[0].(map[string]any)
	if row["id"] != res2.PublicID {
		t.Errorf("remaining queued id = %v, want %s", row["id"], res2.PublicID)
	}
}
