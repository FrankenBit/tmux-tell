// Package identity resolves the calling agent's name from the
// available context (an explicit override, $TMUX_AGENT_NAME — falling
// back to the deprecated $CLAUDE_AGENT_NAME — or the pane→registry
// lookup via $TMUX_PANE).
//
// Both the CLI subcommands and the MCP server go through the same
// helper so the precedence rules stay consistent: changing the
// resolution order in one place changes it everywhere.
package identity

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// Agent-name environment variables (#177 PR2). The substrate reads the
// canonical TMUX_AGENT_NAME, falling back to the deprecated
// CLAUDE_AGENT_NAME for the deprecation cycle (removed envVarRemoval).
const (
	envAgentName       = "TMUX_AGENT_NAME"
	legacyEnvAgentName = "CLAUDE_AGENT_NAME"
	envVarRemoval      = "v0.11.0"
)

// deprecationWarnWriter is where the legacy-env-var deprecation WARN goes, and
// deprecationWarnOnce keeps it to one line per process (identity resolves on
// every CLI invocation and every MCP tool call — without the guard a long-lived
// mcp server would repeat the WARN on each call). Package-level + swappable so
// the white-box tests can capture the output and reset the once.
var (
	deprecationWarnWriter io.Writer = os.Stderr
	deprecationWarnOnce             = &sync.Once{}
)

// envAgentNameValue reads the agent name from the environment, preferring the
// canonical TMUX_AGENT_NAME and falling back to the deprecated
// CLAUDE_AGENT_NAME. When only the legacy var is set it emits the ADR-0008
// deprecation WARN (once per process), matching PR1's claude-msg-alias format.
func envAgentNameValue() string {
	if name := os.Getenv(envAgentName); name != "" {
		return name
	}
	if name := os.Getenv(legacyEnvAgentName); name != "" {
		deprecationWarnOnce.Do(func() {
			fmt.Fprintf(deprecationWarnWriter,
				"WARN deprecated_surface_used name=%s removal=%s — set %s instead (ADR-0008)\n",
				legacyEnvAgentName, envVarRemoval, envAgentName)
		})
		return name
	}
	return ""
}

// Source describes where Resolve found the identity. Useful for
// whoami output and for crafting precise error messages.
type Source string

const (
	// SourceExplicit means the caller passed a non-empty override
	// (typically a --from flag). Highest precedence.
	SourceExplicit Source = "explicit"
	// SourceEnv means an agent-name env var provided the identity —
	// $TMUX_AGENT_NAME, or the deprecated $CLAUDE_AGENT_NAME fallback.
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
//  2. $TMUX_AGENT_NAME (or the deprecated $CLAUDE_AGENT_NAME fallback) —
//     explicit env override, useful for tests and non-tmux contexts.
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
	if name := envAgentNameValue(); name != "" {
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
