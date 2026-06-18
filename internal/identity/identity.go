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

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// Agent-name environment variables (#177 PR2). The substrate reads the
// canonical TMUX_AGENT_NAME, falling back to the deprecated
// CLAUDE_AGENT_NAME for the deprecation cycle (removed envVarRemoval).
const (
	envAgentName       = "TMUX_AGENT_NAME"
	legacyEnvAgentName = "CLAUDE_AGENT_NAME"
	envVarRemoval      = "v1.0" // extended from v0.11.0 per ADR-0008 §Discretion clause (operator decision 2026-06-08)
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

// mismatchWarnWriter / mismatchWarnOnce surface (once per process) that a
// name-env pin ($TMUX_AGENT_NAME) disagrees with the identity resolved from a
// registered pane — a stale-pin signal (#549 Fix-1b). The pane wins (it is the
// re-register-reachable truth), but the conflict is logged loudly so the
// operator clears the pin. Once-per-process because the long-lived MCP server's
// env is fixed for its lifetime: the same mismatch would otherwise repeat on
// every tool call. Swappable for the white-box test, like the deprecation pair.
var (
	mismatchWarnWriter io.Writer = os.Stderr
	mismatchWarnOnce             = &sync.Once{}
)

// WarnMismatch logs (once per process) that the $TMUX_AGENT_NAME pin disagrees
// with an identity resolved from a registered pane, instructing the operator to
// clear the stale pin. No-op when there is no real conflict (either side empty,
// or they agree), so callers can invoke it unconditionally. Exported so the MCP
// ancestor-walk resolver can report the same conflict it detects between an own
// name-pin and an ancestor's registered pane (#549 Fix-1b / #553).
func WarnMismatch(pinName, resolvedName string) {
	if pinName == "" || resolvedName == "" || pinName == resolvedName {
		return
	}
	mismatchWarnOnce.Do(func() {
		fmt.Fprintf(mismatchWarnWriter,
			"WARN identity_mismatch pin=$TMUX_AGENT_NAME=%s resolved=%s — the pane registration wins; clear the stale pin (re-run install or unset $TMUX_AGENT_NAME) (#549)\n",
			pinName, resolvedName)
	})
}

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
//  2. $TMUX_PANE → agents.pane_id → agent name. The registered pane is the
//     authoritative, re-register-reachable identity: tmux sets TMUX_PANE for
//     every pane it spawns, claude inherits it, and discover / manual upsert
//     populates the registry. A `register --force <new-name>` updates this.
//  3. $TMUX_AGENT_NAME (or the deprecated $CLAUDE_AGENT_NAME fallback) —
//     a name pin, used only when the pane does NOT resolve to a registered
//     agent (tests, non-tmux contexts, or a codex MCP child whose pane env was
//     dropped — see resolveMCPIdentity's ancestor walk).
//
// Precedence flip (#549 Fix-1b): the registered pane now outranks the name pin.
// The pin was checked first historically, but a baked/stale $TMUX_AGENT_NAME
// (e.g. a codex global-config pin) could then win over a fresh re-registration —
// the chamber kept sending under its old identity until restart. When BOTH a
// registered pane and a differing pin are present, the pane wins and WarnMismatch
// surfaces the stale pin. The pin only wins when the pane is unregistered (its
// legitimate bootstrap role).
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
	// Read the pin up front (this also fires the legacy-var deprecation WARN, as
	// before), but only USE it as a fallback or for the mismatch check below.
	pinName := envAgentNameValue()
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		agents, err := s.ListAgents(ctx)
		if err != nil {
			return "", SourceNone, err
		}
		for _, a := range agents {
			if a.PaneID == pane {
				// Registered pane wins. If a pin disagrees, it is stale — the
				// pane is the re-register-reachable truth; surface the conflict.
				WarnMismatch(pinName, a.Name)
				return a.Name, SourcePane, nil
			}
		}
		// Pane present but unregistered: fall through to the pin (its bootstrap
		// role) or SourceNone.
	}
	if pinName != "" {
		return pinName, SourceEnv, nil
	}
	return "", SourceNone, nil
}
