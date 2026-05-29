// Package control defines the whitelist of Claude Code slash-commands
// that agents are allowed to invoke through cli-semaphore — split by
// scope: which commands an agent may target at *itself* (self) vs which
// commands an agent may target at *another* agent (peer).
//
// The split matters because the two scopes have very different blast
// radii. A self-`/compact` is the agent quietly trimming its own
// context at a quiescent point. A peer-`/compact` would wipe somebody
// else's working state mid-task. We default to "self-allowed,
// peer-denied" and only flip peer=true for commands whose effect on the
// recipient is benign or actively useful (e.g. rename, help).
//
// New commands and new scope-flips require a code change — the goal is
// to keep the audit surface tiny so an agent can never, say, /clear
// another agent's history or shell out via /bash.
package control

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Scope describes who is targeting whom. ScopeSelf = sender == recipient;
// ScopePeer = sender targeting another agent. The MCP handler picks one
// before calling Resolve.
type Scope int

const (
	ScopeSelf Scope = iota
	ScopePeer
)

func (s Scope) String() string {
	switch s {
	case ScopeSelf:
		return "self"
	case ScopePeer:
		return "peer"
	default:
		return "?"
	}
}

// Command is one entry in the whitelist.
type Command struct {
	// Text is the literal slash-command typed into the recipient pane.
	Text string
	// Self is true when the command may be invoked on $self.
	Self bool
	// Peer is true when the command may be invoked on another agent.
	Peer bool
}

// Allowed lists every recognised command. Keys are the bare name (no
// leading slash). Defaults are intentionally conservative: peer flips
// to true only where the recipient-side effect is benign.
var Allowed = map[string]Command{
	"compact": {Text: "/compact", Self: true, Peer: false},
	"rename":  {Text: "/rename", Self: true, Peer: true},
	"cost":    {Text: "/cost", Self: true, Peer: false},
	"help":    {Text: "/help", Self: true, Peer: true},
	// MCP-server lifecycle: useful after deploying a new semaphore tool
	// so a running agent can refresh its tool surface without losing
	// session context. Disruption surface is small (a brief tool-list
	// flicker, no context loss), so peer-invocation is enabled.
	// Usage: try `mcp-enable-semaphore` first; if Claude Code's `/mcp
	// enable` on an already-enabled server doesn't reconnect, chain
	// `mcp-disable-semaphore` then `mcp-enable-semaphore`.
	"mcp-disable-semaphore": {Text: "/mcp disable semaphore", Self: true, Peer: true},
	"mcp-enable-semaphore":  {Text: "/mcp enable semaphore", Self: true, Peer: true},
}

// ErrNotAllowed is returned by Resolve when the requested command is
// not on the whitelist at all.
var ErrNotAllowed = errors.New("control: command not on whitelist")

// ErrScopeDenied is returned by Resolve when the command exists but is
// not permitted in the requested scope (e.g. peer-invoking a self-only
// command).
var ErrScopeDenied = errors.New("control: command not allowed in this scope")

// Resolve normalises name (trim, lowercase, strip leading slash),
// verifies the command is whitelisted in scope, and returns the literal
// text to send. Two distinct errors so callers can craft a precise
// message: ErrNotAllowed for "unknown command", ErrScopeDenied for
// "exists, but you can't aim it that way".
func Resolve(name string, scope Scope) (string, error) {
	n := strings.TrimSpace(name)
	n = strings.TrimPrefix(n, "/")
	n = strings.ToLower(n)
	if n == "" {
		return "", ErrNotAllowed
	}
	cmd, ok := Allowed[n]
	if !ok {
		return "", ErrNotAllowed
	}
	switch scope {
	case ScopeSelf:
		if !cmd.Self {
			return "", fmt.Errorf("%w: %q is peer-only", ErrScopeDenied, n)
		}
	case ScopePeer:
		if !cmd.Peer {
			return "", fmt.Errorf("%w: %q is self-only", ErrScopeDenied, n)
		}
	}
	return cmd.Text, nil
}

// Names returns every whitelisted command name, sorted — for help text
// and error messages.
func Names() []string {
	out := make([]string, 0, len(Allowed))
	for k := range Allowed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NamesForScope returns the subset of whitelisted commands invokable in
// the given scope.
func NamesForScope(scope Scope) []string {
	out := []string{}
	for k, c := range Allowed {
		ok := (scope == ScopeSelf && c.Self) || (scope == ScopePeer && c.Peer)
		if ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
