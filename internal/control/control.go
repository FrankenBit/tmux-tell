// Package control defines the whitelist of Claude Code slash-commands that
// agents are allowed to invoke on one another through cli-semaphore.
//
// The whitelist is intentionally short and operator-curated. New commands
// require a code change — the goal is to keep blast radius small so an
// agent can never, say, /clear another agent's context or run arbitrary
// shell through /bash.
//
// Commands here must:
//   - Be idempotent or trivially recoverable (a stray /compact is fine; a
//     stray /clear is not).
//   - Have no destructive side effects on conversation history or files.
//   - Take no required arguments — the renderer only sends "/name" with no
//     trailing payload.
package control

import (
	"errors"
	"sort"
	"strings"
)

// Allowed lists the slash-commands an agent may invoke on a peer. Keys are
// the bare command name (no leading "/"). The string value is the literal
// text that will be sent to the recipient pane (always "/name").
var Allowed = map[string]string{
	"compact": "/compact",
	"rename":  "/rename",
	"cost":    "/cost",
	"help":    "/help",
}

// ErrNotAllowed is returned by Resolve when the requested command is not
// on the whitelist.
var ErrNotAllowed = errors.New("control: command not on whitelist")

// Resolve normalises name (trim, lowercase, strip leading slash) and
// returns the literal text to send. Unknown commands return ErrNotAllowed
// wrapped with the rejected name so callers can include it in user-facing
// errors.
func Resolve(name string) (string, error) {
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
	return cmd, nil
}

// Names returns the whitelist as a sorted slice — handy for help text
// and error messages.
func Names() []string {
	out := make([]string, 0, len(Allowed))
	for k := range Allowed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
