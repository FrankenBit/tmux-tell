package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// driveServer runs Serve against the given input lines and returns the
// emitted response lines for assertion.
func driveServer(t *testing.T, s *Server, requests []string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	var outBuf bytes.Buffer
	if err := s.Serve(context.Background(), in, &outBuf); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Serve: %v", err)
	}
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(outBuf.Bytes()), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(line, &v); err != nil {
			t.Fatalf("decode response line %q: %v", line, err)
		}
		out = append(out, v)
	}
	return out
}

func TestServer_InitializeHandshake(t *testing.T) {
	s := NewServer("test-server", "0.0.1")
	got := driveServer(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
	})
	if len(got) != 1 {
		t.Fatalf("responses = %d, want 1", len(got))
	}
	resp := got[0]
	if resp["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v", resp["jsonrpc"])
	}
	result, _ := resp["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", result["protocolVersion"], ProtocolVersion)
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "test-server" {
		t.Errorf("serverInfo.name = %v", info["name"])
	}
}

func TestServer_InitializedNotification(t *testing.T) {
	s := NewServer("t", "0")
	// Notification (no id) → no response.
	got := driveServer(t, s, []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	})
	if len(got) != 0 {
		t.Errorf("got %v, want no response", got)
	}
}

func TestServer_ToolsList(t *testing.T) {
	s := NewServer("t", "0")
	s.RegisterTool("alpha", "first tool",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil })
	s.RegisterTool("beta", "second tool",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil })

	got := driveServer(t, s, []string{
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	})
	result, _ := got[0]["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(tools))
	}
	first, _ := tools[0].(map[string]any)
	if first["name"] != "alpha" {
		t.Errorf("first tool = %v, want alpha (sorted)", first["name"])
	}
}

func TestServer_ToolsCall_HappyPath(t *testing.T) {
	s := NewServer("t", "0")
	s.RegisterTool("echo", "echoes the input",
		json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, args json.RawMessage) (any, error) {
			return map[string]any{"echo": string(args)}, nil
		})

	got := driveServer(t, s, []string{
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"hello":"world"}}}`,
	})
	result, _ := got[0]["result"].(map[string]any)
	if result["isError"] == true {
		t.Errorf("isError = true; got %v", result)
	}
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %d", len(content))
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	// The handler returns {echo: <stringified args>}; the outer text is
	// pretty-printed JSON of that map, so the inner JSON is escaped.
	if !strings.Contains(text, `"echo"`) || !strings.Contains(text, "hello") || !strings.Contains(text, "world") {
		t.Errorf("text doesn't echo args: %s", text)
	}
}

func TestServer_ToolsCall_UnknownTool(t *testing.T) {
	s := NewServer("t", "0")
	got := driveServer(t, s, []string{
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ghost"}}`,
	})
	errObj, _ := got[0]["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("no error in response: %v", got[0])
	}
	if int(errObj["code"].(float64)) != ErrCodeMethodNotFound {
		t.Errorf("code = %v, want MethodNotFound", errObj["code"])
	}
}

func TestServer_ToolsCall_ToolError(t *testing.T) {
	s := NewServer("t", "0")
	s.RegisterTool("fail", "always fails",
		json.RawMessage(`{}`),
		func(_ context.Context, _ json.RawMessage) (any, error) {
			return nil, errors.New("nope")
		})
	got := driveServer(t, s, []string{
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"fail"}}`,
	})
	result, _ := got[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("isError = %v, want true", result["isError"])
	}
}

func TestServer_UnknownMethod(t *testing.T) {
	s := NewServer("t", "0")
	got := driveServer(t, s, []string{
		`{"jsonrpc":"2.0","id":6,"method":"nope/wat"}`,
	})
	errObj, _ := got[0]["error"].(map[string]any)
	if errObj == nil || int(errObj["code"].(float64)) != ErrCodeMethodNotFound {
		t.Errorf("got %v", got[0])
	}
}

func TestServer_MalformedJSONReturnsParseError(t *testing.T) {
	s := NewServer("t", "0")
	got := driveServer(t, s, []string{`{ not json`})
	errObj, _ := got[0]["error"].(map[string]any)
	if errObj == nil || int(errObj["code"].(float64)) != ErrCodeParseError {
		t.Errorf("got %v", got[0])
	}
}
