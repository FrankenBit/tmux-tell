package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
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
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")

	got := callMCPTool(t, s, "semaphore.send", map[string]any{
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
}

func TestMCP_Send_UnknownRecipient(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice")

	got := callMCPTool(t, s, "semaphore.send", map[string]any{
		"to":   "ghost",
		"body": "hi",
	})
	if got["_isError"] != true {
		t.Errorf("isError should be true; got=%v", got)
	}
}

func TestMCP_Send_RequiresEnvVar(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "")
	s := newCmdTestStore(t, "bob")

	got := callMCPTool(t, s, "semaphore.send", map[string]any{
		"to":   "bob",
		"body": "x",
	})
	if got["_isError"] != true {
		t.Errorf("missing env should be error; got=%v", got)
	}
}

func TestMCP_Whoami(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice")

	got := callMCPTool(t, s, "semaphore.whoami", map[string]any{})
	if got["ok"] != true || got["name"] != "alice" {
		t.Errorf("got %v", got)
	}
}

func TestMCP_Agents(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob", "carol")

	got := callMCPTool(t, s, "semaphore.agents", map[string]any{})
	arr, ok := got["_array"].([]any)
	if !ok {
		t.Fatalf("expected array result, got %v", got)
	}
	if len(arr) != 3 {
		t.Errorf("agents = %d, want 3", len(arr))
	}
}

func TestMCP_Inbox(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_NAME", "bob")
	s := newCmdTestStore(t, "alice", "bob")
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "queued"})

	got := callMCPTool(t, s, "semaphore.inbox", map[string]any{})
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
	got := callMCPTool(t, s, "semaphore.status", map[string]any{})
	arr, ok := got["_array"].([]any)
	if !ok {
		t.Fatalf("expected array, got %v", got)
	}
	if len(arr) != 2 {
		t.Errorf("status rows = %d, want 2", len(arr))
	}
}

// listToolsCovers the documented contract: every tool advertised has a
// matching schema entry.
func TestMCP_ToolsListReturnsAllFive(t *testing.T) {
	s := newCmdTestStore(t)
	srv := newMCPServer(s)
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	_ = srv.Serve(context.Background(), in, &out)
	var resp map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp)
	result := resp["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 5 {
		t.Errorf("tools = %d, want 5", len(tools))
	}
	names := map[string]bool{}
	for _, t := range tools {
		names[t.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{
		"semaphore.send", "semaphore.agents", "semaphore.whoami",
		"semaphore.inbox", "semaphore.status",
	} {
		if !names[want] {
			t.Errorf("missing tool: %s", want)
		}
	}
}
