// Package control defines the whitelist of Claude Code slash-commands
// that agents are allowed to invoke through tmux-msg — split by
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
// A third tier — per-edge allowlist — sits between "peer-denied" and
// "peer-allowed-to-all": some commands are too destructive for global
// peer permission but are legitimately useful between specific
// (sender, recipient) pairs. PeerEdges encodes those exceptions:
// `/clear` is denied by default, but Bosun→Pilot is allowed because
// Pilot occasionally hits token exhaustion where /compact can't
// recover and only /clear restores a usable session (#60).
//
// New commands, scope-flips, AND edge rules all require a code change
// — the goal is to keep the audit surface tiny so an agent can never,
// e.g., /clear another agent's history without an explicit, reviewed
// exception or shell out via /bash.
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
// to true only where the recipient-side effect is benign. Per-edge
// exceptions for destructive commands live in PeerEdges below.
var Allowed = map[string]Command{
	"compact": {Text: "/compact", Self: true, Peer: false},
	"rename":  {Text: "/rename", Self: true, Peer: true},
	"cost":    {Text: "/cost", Self: true, Peer: false},
	"help":    {Text: "/help", Self: true, Peer: true},
	// /clear discards all session state. Globally denied (Self: false
	// because a token-exhausted pane can't reach the MCP to call it
	// anyway; Peer: false because anyone-to-anyone clearing is a
	// blast-radius nightmare). The Bosun→Pilot rescue case is allowed
	// via PeerEdges instead — see #60.
	"clear": {Text: "/clear", Self: false, Peer: false},
	// MCP-server lifecycle: useful after deploying a new tmux-msg tool
	// so a running agent can refresh its tool surface without losing
	// session context.
	//
	// Scope split: raw disable is self-only because a peer-DoS via
	// repeated /mcp disable would silently cut another agent off the
	// bus. Enable stays peer-allowed (re-enabling someone is helpful).
	// For the legitimate "peer asks me to restart your MCP" case,
	// callers use the mcp-restart-tmux-msg macro below, which the
	// MCP handler synthesises into disable+enable internally.
	"mcp-disable-tmux-msg": {Text: "/mcp disable tmux-msg", Self: true, Peer: false},
	"mcp-enable-tmux-msg":  {Text: "/mcp enable tmux-msg", Self: true, Peer: true},
	// mcp-restart-tmux-msg is a *macro*. The Text field documents what
	// the macro represents but is not actually typed into a pane —
	// Claude Code has no `/mcp restart` slash command. The MCP handler
	// detects this command by name (or by matching this Text as a
	// sentinel) and queues two control rows: `/mcp disable tmux-msg`
	// then `/mcp enable tmux-msg`. Peer-allowed because the synthesised
	// rows always restore the connection; the raw disable that would
	// expose the DoS surface is never exposed to peers directly.
	"mcp-restart-tmux-msg": {Text: "/mcp restart tmux-msg", Self: true, Peer: true},
}

// Edge identifies a specific (sender → recipient) pair for which a
// per-edge exception applies. From/To are matched against the canonical
// agent names from the tmux-msg registry — exact match, no wildcards
// in this first cut (#60).
type Edge struct {
	From string // sender agent name
	To   string // recipient agent name
}

// PeerEdges is the per-edge exception layer for commands whose global
// Peer flag is false. Keys are the bare command name (matching Allowed).
// When Resolve encounters a peer scope and the command's Peer flag is
// false, it falls through to a PeerEdges lookup before returning
// ErrScopeDenied — if any edge in the slice matches (sender, recipient)
// exactly, the command is permitted.
//
// Adding an edge is the same audit-surface event as flipping Peer=true
// for a single sender/recipient pair: it grants peer invocation, just
// narrowly. New edges require a code change so the policy expansion is
// reviewable.
//
// Maintenance: From/To values are matched against canonical agent
// names from the tmux-msg registry. When a agent is renamed (e.g.,
// the 2026-06-02 Admin → Quartermaster rename), every PeerEdges entry
// referencing the old name MUST be updated in lockstep — otherwise
// the edge silently stops matching and the rescue path goes dark.
// Surveyor S2 forward-watch on PR #61.
var PeerEdges = map[string][]Edge{
	// Bosun → Pilot and Quartermaster → Pilot routine-clear /
	// rescue paths. Two motivations converge on the same edge shape:
	//   - Rescue: Pilot occasionally hits token exhaustion where
	//     /compact can't recover, and only /clear restores a usable
	//     session. The destructive cost (loses in-flight work) is
	//     accepted because the alternative is a dead session (#60).
	//   - Routine: Pilot's intended lifecycle is clear-before-each-task
	//     (feedback_pilot_clear_before_each_task), so its dispatcher
	//     fires /clear ahead of a fresh dispatch as standard cadence.
	// Both Bosun (#60) and Quartermaster (#167) are established
	// dispatchers into that lifecycle, so each gets the edge. The edge
	// stays narrow (specific sender → pilot) rather than a global
	// Peer=true: anyone-to-anyone /clear is a blast-radius nightmare,
	// and other senders (Engineer, Shipwright) get their own edges only
	// if/when those dispatch patterns emerge — conservative-default-
	// with-explicit-opt-in (this package's doc-comment).
	"clear": {
		{From: "bosun", To: "pilot"},
		{From: "quartermaster", To: "pilot"},
	},
}

// ErrNotAllowed is returned by Resolve when the requested command is
// not on the whitelist at all.
var ErrNotAllowed = errors.New("control: command not on whitelist")

// ErrScopeDenied is returned by Resolve when the command exists but is
// not permitted in the requested scope (e.g. peer-invoking a self-only
// command, or peer-invoking a globally-denied command from a sender
// that has no PeerEdge to the recipient).
var ErrScopeDenied = errors.New("control: command not allowed in this scope")

// Resolve normalises name (trim, lowercase, strip leading slash),
// verifies the command is whitelisted in scope, and returns the literal
// text to send. The (sender, recipient) names are matched against
// PeerEdges when the requested scope is peer and the command's global
// Peer flag is false — they MUST be passed even for self-scope (where
// they're identical) so the function signature stays uniform.
//
// Two distinct errors so callers can craft a precise message:
// ErrNotAllowed for "unknown command", ErrScopeDenied for "exists, but
// you can't aim it that way (or you specifically aren't on the
// per-edge allowlist)".
func Resolve(name string, scope Scope, sender, recipient string) (string, error) {
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
			// Pre-#60 every Self=false entry had Peer=true, so the
			// historical "is peer-only" wording was always accurate.
			// /clear breaks that: Self=false AND Peer=false (the edge
			// layer is the only path through). Differentiate so the
			// error tells the caller what WOULD have let it through.
			// Surveyor S1 absorb on PR #61.
			switch {
			case cmd.Peer:
				return "", fmt.Errorf("%w: %q is peer-only", ErrScopeDenied, n)
			case len(PeerEdges[n]) > 0:
				return "", fmt.Errorf("%w: %q is restricted to specific peer (sender, recipient) edges; not self-invokable",
					ErrScopeDenied, n)
			default:
				return "", fmt.Errorf("%w: %q is not invokable in any scope", ErrScopeDenied, n)
			}
		}
	case ScopePeer:
		if cmd.Peer {
			return cmd.Text, nil
		}
		// Global peer-denied — fall through to PeerEdges. An exact
		// (sender, recipient) match grants the invocation narrowly.
		edges := PeerEdges[n]
		for _, e := range edges {
			if e.From == sender && e.To == recipient {
				return cmd.Text, nil
			}
		}
		if len(edges) == 0 {
			// No edge layer at all → command is unconditionally
			// self-only. Keep the historical wording so callers'
			// error rendering stays stable.
			return "", fmt.Errorf("%w: %q is self-only", ErrScopeDenied, n)
		}
		// Edge layer exists but this (sender, recipient) pair isn't
		// on it. The more specific wording surfaces the routing
		// context so the caller knows it's an edge-mismatch, not a
		// scope mismatch.
		return "", fmt.Errorf("%w: %q not invokable from %q to %q",
			ErrScopeDenied, n, sender, recipient)
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
// the given scope for the given (sender, recipient) pair. For peer
// scope, this includes both globally peer-allowed commands AND any
// commands granted via a matching PeerEdge — so the error message
// listing "what you CAN invoke" stays accurate when the caller is on
// an edge-exception path. (sender, recipient) are ignored for self
// scope.
func NamesForScope(scope Scope, sender, recipient string) []string {
	out := []string{}
	for k, c := range Allowed {
		switch scope {
		case ScopeSelf:
			if c.Self {
				out = append(out, k)
			}
		case ScopePeer:
			if c.Peer {
				out = append(out, k)
				continue
			}
			// Per-edge exceptions are also "invokable in peer scope"
			// for callers who match an edge — surface those names too.
			for _, e := range PeerEdges[k] {
				if e.From == sender && e.To == recipient {
					out = append(out, k)
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out
}
