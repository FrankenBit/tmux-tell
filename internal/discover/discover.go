// Package discover resolves tmux panes to tmux-msg agent names. It is
// the shared engine behind:
//
//   - the `claude-msg discover` subcommand (one-shot rewrite of pane_id),
//   - the mailman's auto-heal path (re-resolve after "can't find pane"),
//   - and future tools that need to ask "which pane runs agent X right now".
//
// The resolution strategy walks each pane's process tree looking for
// `claude --resume <name>` in argv (cmdline), then falls back to the pane
// title with its Claude-status-indicator prefix stripped.
package discover

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// ClaudeSessionIDEnv is the process-env var Claude Code exports carrying the
// session UUID. Read from /proc/<pid>/environ to resolve a pane's intrinsic
// session identity, the primary key for session-as-addressee (#626 Phase 1b).
const ClaudeSessionIDEnv = "CLAUDE_CODE_SESSION_ID"

// Source describes which strategy produced an agent name. Used for tests
// and so callers can log the resolution path.
type Source string

const (
	SourceCmdline    Source = "cmdline"     // claude --resume <name> found in argv
	SourceTitle      Source = "pane_title"  // pane title used as the name
	SourceWindowName Source = "window_name" // window name used as the name
)

// Resolved is one pane → agent mapping the walker produced.
type Resolved struct {
	PaneID    string
	AgentName string
	Source    Source
}

// CmdlineReader reads /proc/<pid>/cmdline. Swappable for tests.
type CmdlineReader func(pid int) (string, error)

// ChildrenReader returns the direct child PIDs of parent. Swappable for tests.
type ChildrenReader func(parent int) []int

// EnvironReader reads a single env var (key) from /proc/<pid>/environ.
// Swappable for tests. Returns ("", false) when the var is absent or the
// process can't be read. A nil EnvironReader disables session-id discovery
// (the walker falls back to name-only resolution) — existing callers that
// build a Walker without one keep working unchanged.
type EnvironReader func(pid int, key string) (string, bool)

// Walker holds the resolution strategy. Construct with New for production
// defaults; tests build one with the fields set explicitly.
type Walker struct {
	CmdlineReader  CmdlineReader
	ChildrenReader ChildrenReader
	// EnvironReader reads /proc/<pid>/environ for a key (#626 Phase 1b
	// session-id discovery). Nil = session-id discovery disabled.
	EnvironReader EnvironReader
	// MaxDepth bounds the descendant walk per pane (0 = pane root only).
	// Default is 3 — covers bash → claude → MCP-shim style trees.
	MaxDepth int
}

// New returns a Walker with production /proc and pgrep-style readers.
func New() *Walker {
	return &Walker{
		CmdlineReader:  DefaultCmdlineReader,
		ChildrenReader: DefaultChildrenReader,
		EnvironReader:  DefaultEnvironReader,
		MaxDepth:       3,
	}
}

// DefaultEnvironReader reads /proc/<pid>/environ and returns the value for key.
func DefaultEnvironReader(pid int, key string) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return "", false
	}
	prefix := []byte(key + "=")
	for _, item := range bytes.Split(data, []byte{0}) {
		if v, ok := bytes.CutPrefix(item, prefix); ok {
			return string(v), true
		}
	}
	return "", false
}

// DefaultCmdlineReader reads /proc/<pid>/cmdline.
func DefaultCmdlineReader(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DefaultChildrenReader returns immediate child PIDs by reading
// /proc/<pid>/task/<pid>/children.
func DefaultChildrenReader(parent int) []int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", parent, parent))
	if err != nil {
		return nil
	}
	var pids []int
	for _, tok := range strings.Fields(string(b)) {
		var n int
		if _, err := fmt.Sscanf(tok, "%d", &n); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// Resolve produces a Resolved for one pane, or returns ok=false if no name
// could be derived.
func (w *Walker) Resolve(p tmuxio.PaneInfo) (Resolved, bool) {
	if name, ok := w.cmdlineDescendantSearch(p.PID, 0); ok {
		return Resolved{PaneID: p.ID, AgentName: name, Source: SourceCmdline}, true
	}
	if name := stripTitleIndicators(p.Title); name != "" {
		return Resolved{PaneID: p.ID, AgentName: name, Source: SourceTitle}, true
	}
	if name := stripTitleIndicators(p.WindowName); name != "" && !isGenericWindowName(name) {
		return Resolved{PaneID: p.ID, AgentName: name, Source: SourceWindowName}, true
	}
	return Resolved{}, false
}

// WalkAll returns the resolution result for every visible tmux pane.
func (w *Walker) WalkAll(ctx context.Context) ([]Resolved, error) {
	panes, err := tmuxio.ListPanesWithPID(ctx)
	if err != nil {
		return nil, err
	}
	var out []Resolved
	for _, p := range panes {
		if r, ok := w.Resolve(p); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// CanonicalAgent is one canonical (registered) agent name plus its
// alias list. Callers supply a slice of these to LookupByName /
// PaneAgentName when they want canonical-name resolution per #38.
type CanonicalAgent struct {
	Name    string
	Aliases []string
}

// LookupByName returns the current pane id for an agent, or "" if no
// pane matches. Used by the mailman's auto-heal path.
func (w *Walker) LookupByName(ctx context.Context, name string) (string, error) {
	all, err := w.WalkAll(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range all {
		if r.AgentName == name {
			return r.PaneID, nil
		}
	}
	return "", nil
}

// LookupByNameWithCanonicals finds a pane running the requested
// canonical agent. Matching is:
//
//  1. Exact match on the canonical name.
//  2. Exact match on any of the agent's aliases.
//  3. Case-insensitive substring match: any canonical name appears
//     in the running --resume value (e.g. `bosun` ↔ `Master Bosun
//     of Nimbus`). Returns ambiguous=true if two canonicals both
//     substring-match the same pane.
//
// `canonicals` is typically the result of `store.ListAgents` filtered
// to just (Name, Aliases). The caller passes this so the discover
// package stays storage-agnostic.
//
// Returns paneID, ambiguous, error. ambiguous=true with paneID="" tells
// the caller "we found a pane but it could be more than one canonical;
// skip rather than guess."
func (w *Walker) LookupByNameWithCanonicals(ctx context.Context, name string, canonicals []CanonicalAgent) (string, bool, error) {
	all, err := w.WalkAll(ctx)
	if err != nil {
		return "", false, err
	}
	// Pass 1: exact match (canonical name or any alias). Surveyor
	// review of v0.2.0 — Q(a) — flagged that collecting only the
	// first hit was wrong because two canonicals can both exact-match
	// the same running value (e.g. canonical "admin" has alias
	// "claude" AND canonical "pilot" has alias "claude"). The
	// registration-time check in store.AddAlias should prevent that
	// from ever being stored, but defence-in-depth: ambiguous=true
	// even on exact matches when >1 canonical claims the running name.
	for _, r := range all {
		matched := exactMatches(r.AgentName, canonicals)
		switch len(matched) {
		case 0:
			continue
		case 1:
			if matched[0] == name {
				return r.PaneID, false, nil
			}
			// Exact match exists, just not for the requested name.
			// Keep scanning — maybe a later pane matches `name`.
		default:
			// >1 canonical exact-matches this running value. We can't
			// decide; the caller should log + bail.
			return "", true, nil
		}
	}
	// Pass 2: substring match across canonicals. Same ambiguity rule.
	for _, r := range all {
		matched := substringMatches(r.AgentName, canonicals)
		if len(matched) == 0 {
			continue
		}
		if len(matched) > 1 {
			return "", true, nil
		}
		if matched[0] == name {
			return r.PaneID, false, nil
		}
	}
	return "", false, nil
}

// PaneAgentNameWithCanonicals is like PaneAgentName but resolves the
// running pane's --resume value back to a canonical name via the
// supplied canonicals. Used by the mailman's silent-drift check so
// `bosun` matches a pane running `claude --resume "Master Bosun of
// Nimbus"`.
//
// Returns the canonical name, ambiguous, error. Falls back to the
// raw --resume value when no canonical matches — preserves the
// existing PaneAgentName behaviour for callers that don't have
// canonical data.
func (w *Walker) PaneAgentNameWithCanonicals(ctx context.Context, paneID string, canonicals []CanonicalAgent) (string, bool, error) {
	all, err := w.WalkAll(ctx)
	if err != nil {
		return "", false, err
	}
	for _, r := range all {
		if r.PaneID != paneID {
			continue
		}
		// Exact match (canonical name or alias). Q(a) fix: collect
		// ALL canonicals that exact-match, not just the first. If >1,
		// caller bails rather than picking by slice order.
		matched := exactMatches(r.AgentName, canonicals)
		switch len(matched) {
		case 1:
			return matched[0], false, nil
		case 0:
			// Fall through to substring.
		default:
			return "", true, nil
		}
		matched = substringMatches(r.AgentName, canonicals)
		switch len(matched) {
		case 0:
			// No canonical claims this — preserve the raw --resume
			// value so callers without a canonical registry (e.g.
			// tests) still get something usable.
			return r.AgentName, false, nil
		case 1:
			return matched[0], false, nil
		default:
			return "", true, nil
		}
	}
	return "", false, nil
}

// exactMatches returns every canonical whose name OR any alias is
// exactly `running`. Length is the ambiguity signal: 0 = no match,
// 1 = unambiguous, >1 = ambiguous (Q(a) Surveyor review).
func exactMatches(running string, canonicals []CanonicalAgent) []string {
	var out []string
	for _, c := range canonicals {
		if c.Name == running {
			out = append(out, c.Name)
			continue
		}
		for _, alias := range c.Aliases {
			if alias == running {
				out = append(out, c.Name)
				break
			}
		}
	}
	return out
}

func substringMatches(running string, canonicals []CanonicalAgent) []string {
	low := strings.ToLower(running)
	var out []string
	for _, c := range canonicals {
		if strings.Contains(low, strings.ToLower(c.Name)) {
			out = append(out, c.Name)
		}
	}
	return out
}

// PaneAgentName returns the agent name running in the given pane, or
// "" if no name could be derived. Used by the mailman's pre-delivery
// silent-drift check (#37): before paste-buffer-and-Enter, verify
// that the registered pane is still running the expected agent. The
// "registered pane exists but holds a different agent" case is the
// scenario that produced the 2026-05-31 misdelivery — auto-heal
// only fires on "can't find pane" errors, so a pane that exists but
// belongs to someone else slips through silently.
//
// The lookup goes via WalkAll so the resolution strategy stays the
// same as LookupByName: cmdline → title → window_name. Cost: one
// tmux list-panes + one /proc walk for the matched pane.
func (w *Walker) PaneAgentName(ctx context.Context, paneID string) (string, error) {
	all, err := w.WalkAll(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range all {
		if r.PaneID == paneID {
			return r.AgentName, nil
		}
	}
	return "", nil
}

// cmdlineDescendantSearch walks pid + descendants up to MaxDepth looking
// for `claude --resume <name>`. Returns the first match.
func (w *Walker) cmdlineDescendantSearch(pid, depth int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	cmdline, err := w.CmdlineReader(pid)
	if err == nil {
		argv := parseCmdline(cmdline)
		if name := extractResumeName(argv); name != "" {
			return name, true
		}
	}
	if depth >= w.MaxDepth {
		return "", false
	}
	for _, child := range w.ChildrenReader(pid) {
		if name, ok := w.cmdlineDescendantSearch(child, depth+1); ok {
			return name, true
		}
	}
	return "", false
}

// sessionIDDescendantSearch walks pid + descendants up to MaxDepth looking
// for CLAUDE_CODE_SESSION_ID in the process environ. Returns the first
// non-empty value. Mirrors cmdlineDescendantSearch but reads environ. A nil
// EnvironReader (session-id discovery disabled) returns ("", false).
func (w *Walker) sessionIDDescendantSearch(pid, depth int) (string, bool) {
	if pid <= 0 || w.EnvironReader == nil {
		return "", false
	}
	if v, ok := w.EnvironReader(pid, ClaudeSessionIDEnv); ok && v != "" {
		return v, true
	}
	if depth >= w.MaxDepth {
		return "", false
	}
	for _, child := range w.ChildrenReader(pid) {
		if v, ok := w.sessionIDDescendantSearch(child, depth+1); ok {
			return v, true
		}
	}
	return "", false
}

// SessionIDForPane resolves the Claude session UUID hosted in a single pane by
// walking that pane's process tree for CLAUDE_CODE_SESSION_ID. Used at register
// time for self-discovery (#626 Phase 1b). Returns ("", false) when no live
// Claude session is found in the pane (bare shell, non-Claude CLI, env unset,
// or session-id discovery disabled).
func (w *Walker) SessionIDForPane(ctx context.Context, paneID string) (string, bool) {
	panes, err := tmuxio.ListPanesWithPID(ctx)
	if err != nil {
		return "", false
	}
	for _, p := range panes {
		if p.ID == paneID {
			return w.sessionIDDescendantSearch(p.PID, 0)
		}
	}
	return "", false
}

// LookupBySessionID returns the pane id currently hosting the given Claude
// session UUID, or "" if no pane's process tree carries it. The primary
// (exact) resolution path for session-as-addressee (#626 Phase 1b): unlike the
// fuzzy name match it cannot mis-resolve to a different agent, and a bare shell
// / ended session simply isn't found (so the caller blocks rather than pasting
// — composes with the Phase-1a bare-shell guard). Empty sessionID returns "".
func (w *Walker) LookupBySessionID(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return "", nil
	}
	panes, err := tmuxio.ListPanesWithPID(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range panes {
		if v, ok := w.sessionIDDescendantSearch(p.PID, 0); ok && v == sessionID {
			return p.ID, nil
		}
	}
	return "", nil
}

// parseCmdline splits /proc/<pid>/cmdline (NUL-separated) into argv.
func parseCmdline(raw string) []string {
	raw = strings.TrimRight(raw, "\x00")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\x00")
}

// extractResumeName supports both `--resume name` (with embedded spaces
// collected until the next -flag) and `--resume=name`. Returns "" if
// argv doesn't contain a --resume flag.
func extractResumeName(argv []string) string {
	for i, a := range argv {
		if strings.HasPrefix(a, "--resume=") {
			return strings.TrimPrefix(a, "--resume=")
		}
		if a == "--resume" {
			var parts []string
			for j := i + 1; j < len(argv); j++ {
				if strings.HasPrefix(argv[j], "-") {
					break
				}
				parts = append(parts, argv[j])
			}
			return strings.Join(parts, " ")
		}
	}
	return ""
}

// stripTitleIndicators removes leading Unicode marker glyphs Claude Code
// uses in the pane title (⠐, ⠂, ✳, ●, ✓, …) plus surrounding whitespace.
// The heuristic: strip leading non-ASCII runes and whitespace until we hit
// a printable ASCII letter/digit.
func stripTitleIndicators(s string) string {
	for i, r := range s {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return strings.TrimSpace(s[i:])
		}
		// Allow ASCII underscore/hyphen as a starter (unusual but valid).
		if r == '_' || r == '-' {
			return strings.TrimSpace(s[i:])
		}
		if !unicode.IsSpace(r) && r < 128 {
			// Some other ASCII punct — stop stripping, give up.
			break
		}
	}
	return ""
}

// isGenericWindowName filters out tmux's default window names that aren't
// useful agent identifiers ("bash", "claude", "zsh", numeric, …).
func isGenericWindowName(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "bash", "sh", "zsh", "fish", "claude", "tmux":
		return true
	}
	return false
}
