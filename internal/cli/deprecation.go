package cli

import (
	"fmt"
	"io"
	"os"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
)

// Phase-3 deprecation surfacing (#440 / ADR-0008). The substrate rename keeps
// every legacy operator surface working through v1.0 (§Discretion); this file
// is the operator-comm half — once-per-process WARNs at Run entry that name the
// canonical replacement. The path/env *resolution* stays pure in common.go +
// internal/config so the ~many store-open / config-load call sites don't thread
// stderr; the deprecation notice is centralized here alongside the existing
// warnIfDeprecatedName (run.go), the one place with stderr + Run-entry timing.

// warnIfDeprecatedEnv emits a WARN for each deprecated env var that is set,
// naming its canonical replacement. Fires whether or not the new var is also set
// — an operator carrying the old name in their shell rc should be told to update
// it even if the new one happens to win resolution.
func warnIfDeprecatedEnv(stderr io.Writer) {
	for _, e := range []struct{ oldName, newName string }{
		{legacyEnvDB, envDB},         // CLAUDE_MSG_DB     → TMUX_TELL_DB
		{legacyEnvConfig, envConfig}, // CLAUDE_MSG_CONFIG → TMUX_TELL_CONFIG
	} {
		if os.Getenv(e.oldName) != "" {
			fmt.Fprintf(stderr,
				"WARN deprecated_env_var_used name=%s removal=%s — set %s instead (ADR-0008)\n",
				e.oldName, deprecatedRemovalVersion, e.newName)
		}
	}
}

// warnIfLegacyDataPath emits a migration WARN when the DEFAULT resolution falls
// back to a legacy tmux-msg path — naming the verbatim `mv` recipe so it is a
// copy-paste action. Only fires for the default case: an explicit $TMUX_TELL_DB
// / $CLAUDE_MSG_DB means the operator chose a path, so the DB note is suppressed
// when either is set (a narrow caveat: a per-command --db override can't be seen
// from Run, so an explicit --db with a legacy file on disk + no env still notes —
// informational, not an error). config resolution accounts for its env vars
// internally, so config.ResolvePath's legacy flag is authoritative.
func warnIfLegacyDataPath(stderr io.Writer) {
	if os.Getenv(envDB) == "" && os.Getenv(legacyEnvDB) == "" {
		if _, legacy := defaultDBLocationResolved(); legacy {
			fmt.Fprintf(stderr,
				"WARN legacy_data_path_in_use kind=db removal=%s — migrate with: mv %s %s\n",
				deprecatedRemovalVersion, legacyDataDir(), defaultDataDir())
		}
	}
	if _, legacy := config.ResolvePath(); legacy {
		fmt.Fprintf(stderr,
			"WARN legacy_data_path_in_use kind=config removal=%s — migrate with: mv %s %s\n",
			deprecatedRemovalVersion, config.LegacyPath, config.DefaultPath)
	}
}
