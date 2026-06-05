// Package identity resolves the calling agent's name from the
// available context (an explicit override, $CLAUDE_AGENT_NAME, or the
// pane→registry lookup via $TMUX_PANE).
//
// Both the CLI subcommands and the MCP server go through the same
// helper so the precedence rules stay consistent: changing the
// resolution order in one place changes it everywhere.
package identity

import (
	"context"
	"os"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// Source describes where Resolve found the identity. Useful for
// whoami output and for crafting precise error messages.
type Source string

const (
	// SourceExplicit means the caller passed a non-empty override
	// (typically a --from flag). Highest precedence.
	SourceExplicit Source = "explicit"
	// SourceEnv means $CLAUDE_AGENT_NAME provided the identity.
	SourceEnv Source = "env"
	// SourcePane means $TMUX_PANE looked up against agents.pane_id
	// provided the identity. The default for a registered pane.
	SourcePane Source = "pane"
	// SourceNone means no identity could be resolved. Not an error
	// in itself — the caller decides whether that's actionable.
	SourceNone Source = ""
)

// Resolve picks an agent identity from (in order):
//
//  1. override — useful for the CLI's --from flag.
//  2. $CLAUDE_AGENT_NAME — explicit env override, useful for tests
//     and non-tmux contexts.
//  3. $TMUX_PANE → agents.pane_id → agent name. This is the path
//     that makes the bus work with zero per-pane config: tmux already
//     sets TMUX_PANE for every pane it spawns, claude inherits it,
//     and we look it up in the registry that discover / manual upsert
//     has populated.
//
// Returns (name, source). If nothing resolves, returns
// ("", SourceNone, nil) — not an error; the caller crafts the
// actionable "register this pane" / "pass --from" message.
//
// Spoofing note: $TMUX_PANE is settable by anything with shell
// access, and the registry has no per-pane authentication. This
// helper widens *convenience*, it does not authenticate identity.
// The homelab single-operator trust model is unchanged.
func Resolve(ctx context.Context, s *store.Store, override string) (string, Source, error) {
	if override != "" {
		return override, SourceExplicit, nil
	}
	if name := os.Getenv("CLAUDE_AGENT_NAME"); name != "" {
		return name, SourceEnv, nil
	}
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return "", SourceNone, nil
	}
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return "", SourceNone, err
	}
	for _, a := range agents {
		if a.PaneID == pane {
			return a.Name, SourcePane, nil
		}
	}
	return "", SourceNone, nil
}
