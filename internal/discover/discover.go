// Package discover resolves tmux panes to cli-semaphore agent names. It is
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
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

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

// Walker holds the resolution strategy. Construct with New for production
// defaults; tests build one with the fields set explicitly.
type Walker struct {
	CmdlineReader  CmdlineReader
	ChildrenReader ChildrenReader
	// MaxDepth bounds the descendant walk per pane (0 = pane root only).
	// Default is 3 — covers bash → claude → MCP-shim style trees.
	MaxDepth int
}

// New returns a Walker with production /proc and pgrep-style readers.
func New() *Walker {
	return &Walker{
		CmdlineReader:  DefaultCmdlineReader,
		ChildrenReader: DefaultChildrenReader,
		MaxDepth:       3,
	}
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
