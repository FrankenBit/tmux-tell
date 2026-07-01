// Package debug is the single gate for verbose diagnostic output across the
// substrate (#681). Diagnostics-heavy sites that drop a best-effort probe
// error — routine in normal operation, but useful when something is actually
// misbehaving — guard the extra logging on Enabled() so the default output
// stays quiet:
//
//	if debug.Enabled() {
//		log.Printf("evidence-gather: agent %q lookup err=%v", agent, err)
//	}
//
// Scope is α-narrow (#681): only genuinely-diagnostic paths (ping/discover/
// state-classifier/deliver-verify) gate on this, NOT every dropped error in
// internal/* — a broad gate would make the output un-scannable. Widen
// deliberately as new diagnostic surfaces warrant it, not implicitly.
package debug

import "os"

// EnvVar is the environment variable that enables verbose debug output.
const EnvVar = "TMUX_TELL_DEBUG"

// Enabled reports whether verbose debug output is requested. Any non-empty
// value of TMUX_TELL_DEBUG (e.g. "1", "true") turns it on; empty or unset
// leaves it off. Read fresh each call so a test can toggle it via t.Setenv.
func Enabled() bool {
	return os.Getenv(EnvVar) != ""
}
