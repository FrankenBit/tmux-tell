// Package mcp is a minimal Model Context Protocol server speaking JSON-RPC
// 2.0 over stdio. It implements just enough of the spec
// (https://modelcontextprotocol.io) for tmux-msg's needs: the
// initialize handshake, tools/list, and tools/call. The transport is
// line-delimited JSON on stdin/stdout — one JSON object per line.
//
// Tool implementations are registered via Server.RegisterTool. The
// signature is intentionally small so the tmux-msg subcommands
// can be wrapped one-to-one.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersion is the MCP version we advertise during initialize.
// 2024-11-05 is the stable version widely supported by clients.
const ProtocolVersion = "2024-11-05"

// JSON-RPC standard error codes (https://www.jsonrpc.org/specification).
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603
)

// ToolHandler is the function signature for a registered tool. It
// receives the raw arguments object from the client and returns either
// a JSON-serialisable result or an error.
type ToolHandler func(ctx context.Context, args json.RawMessage) (any, error)

// Tool describes a single registered tool the way MCP clients want to see
// it.
type Tool struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	InputSchema json.RawMessage   `json:"inputSchema"`
	Handler     ToolHandler       `json:"-"`
}

// Server is the JSON-RPC dispatcher. Construct one with NewServer, attach
// tools via RegisterTool, then Serve.
type Server struct {
	name    string
	version string
	mu      sync.RWMutex
	tools   map[string]*Tool
}

// NewServer constructs a Server advertising the given name/version in the
// initialize response.
func NewServer(name, version string) *Server {
	return &Server{
		name:    name,
		version: version,
		tools:   map[string]*Tool{},
	}
}

// RegisterTool adds a tool to the server. inputSchema must be a JSON object
// (typically a JSON Schema describing the arguments) provided as a raw
// JSON byte slice so the server doesn't impose an opinionated schema type.
func (s *Server) RegisterTool(name, description string, inputSchema json.RawMessage, handler ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[name] = &Tool{
		Name:        name,
		Description: description,
		InputSchema: inputSchema,
		Handler:     handler,
	}
}

// Serve reads JSON-RPC requests from in, dispatches them, and writes
// responses to out. It returns when in is closed (EOF) or ctx is done.
//
// Notifications (requests with no id) get no response, per JSON-RPC 2.0.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// Allow large messages (long tool responses, big bodies).
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1*1024*1024)
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)

	var encMu sync.Mutex
	writeResponse := func(resp *response) error {
		encMu.Lock()
		defer encMu.Unlock()
		return encoder.Encode(resp)
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Each request is dispatched synchronously for now — keeps the
		// store interactions serial and matches MCP client expectations.
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			if err := writeResponse(errorResp(nil, ErrCodeParseError, "parse error: "+err.Error())); err != nil {
				return err
			}
			continue
		}
		if req.JSONRPC != "" && req.JSONRPC != "2.0" {
			if err := writeResponse(errorResp(req.ID, ErrCodeInvalidRequest, "unsupported jsonrpc version")); err != nil {
				return err
			}
			continue
		}
		resp := s.dispatch(ctx, &req)
		if resp == nil {
			continue // notification — no response
		}
		if err := writeResponse(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (s *Server) dispatch(ctx context.Context, req *request) *response {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "initialized", "notifications/initialized":
		// Notification — no response.
		return nil
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	case "ping":
		return okResp(req.ID, map[string]any{})
	default:
		if req.ID == nil {
			return nil // unknown notification, ignore.
		}
		return errorResp(req.ID, ErrCodeMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *Server) handleInitialize(req *request) *response {
	return okResp(req.ID, map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    s.name,
			"version": s.version,
		},
	})
}

func (s *Server) handleToolsList(req *request) *response {
	s.mu.RLock()
	tools := make([]*Tool, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, t)
	}
	s.mu.RUnlock()
	// Sort for deterministic output.
	sortTools(tools)
	return okResp(req.ID, map[string]any{"tools": tools})
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleToolsCall(ctx context.Context, req *request) *response {
	var p toolsCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errorResp(req.ID, ErrCodeInvalidParams, err.Error())
	}
	if p.Name == "" {
		return errorResp(req.ID, ErrCodeInvalidParams, "tool name required")
	}
	s.mu.RLock()
	tool, ok := s.tools[p.Name]
	s.mu.RUnlock()
	if !ok {
		return errorResp(req.ID, ErrCodeMethodNotFound, "unknown tool: "+p.Name)
	}
	result, err := tool.Handler(ctx, p.Arguments)
	if err != nil {
		// Errors come back as content + isError per MCP spec, so the
		// client can show them to the user instead of treating them as
		// transport errors.
		return okResp(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		})
	}
	// Encode the result as JSON text content. MCP clients can re-decode
	// or display verbatim.
	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errorResp(req.ID, ErrCodeInternalError, err.Error())
	}
	return okResp(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(body)}},
	})
}

// ErrToolFailed is a convenience for tool handlers that want to signal a
// tool-level (not transport-level) failure.
var ErrToolFailed = errors.New("tool failed")

// --- internal wire types ---

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *responseError  `json:"error,omitempty"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func okResp(id json.RawMessage, result any) *response {
	return &response{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResp(id json.RawMessage, code int, msg string) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &responseError{Code: code, Message: msg}}
}

func sortTools(tools []*Tool) {
	// Simple insertion sort — N is small.
	for i := 1; i < len(tools); i++ {
		for j := i; j > 0 && tools[j-1].Name > tools[j].Name; j-- {
			tools[j-1], tools[j] = tools[j], tools[j-1]
		}
	}
}

// Silence unused warnings.
var _ = fmt.Sprintf
