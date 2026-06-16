package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServe_DeprecatedNotifyOnDeliveredUnverifiedFlag verifies that
// --notify-on-delivered-unverified emits WARN deprecated_surface_used and
// maps its value through to the canonical flag (#140).
func TestServe_DeprecatedNotifyOnDeliveredUnverifiedFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantWarn bool
	}{
		{"legacy flag used", []string{"--notify-on-delivered-unverified=false"}, true},
		{"new flag used", []string{"--notify-on-delivered-in-input-box=false"}, false},
		{"neither flag", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// runServeCLI will error (no agent, no db) but the deprecation WARN
			// fires before those checks, so we can observe stderr.
			var stderr bytes.Buffer
			args := append([]string{"--agent=testpilot"}, tc.args...)
			runServeCLI(args, &bytes.Buffer{}, &stderr)
			got := stderr.String()
			hasWarn := strings.Contains(got, "WARN deprecated_surface_used name=--notify-on-delivered-unverified")
			if hasWarn != tc.wantWarn {
				t.Errorf("args=%v: warn=%v, want %v (stderr=%q)", tc.args, hasWarn, tc.wantWarn, got)
			}
			if tc.wantWarn {
				for _, want := range []string{"removal=v1.0", "notify-on-delivered-in-input-box"} {
					if !strings.Contains(got, want) {
						t.Errorf("warn missing %q; got %q", want, got)
					}
				}
			}
		})
	}
}

// TestWarnIfDeprecatedName exercises the default (claude) profile's alias CHAIN
// (#440 Phase 3): both the older claude-msg (#177) and the rename-leg
// tmux-msg-claude WARN → the canonical tmux-tell-claude. The tmux-msg-claude case
// is the load-bearing backward-compat pin — drop it from the claude
// DeprecatedAliases list and that subcase goes red (mutation-verified).
func TestWarnIfDeprecatedName(t *testing.T) {
	cases := []struct {
		name     string
		argv0    string
		wantName string // the alias the WARN must name; "" => no WARN
	}{
		{"older alias claude-msg full path", "/usr/local/bin/claude-msg", "claude-msg"},
		{"older alias claude-msg bare", "claude-msg", "claude-msg"},
		{"rename leg tmux-msg-claude full path", "/usr/local/bin/tmux-msg-claude", "tmux-msg-claude"},
		{"rename leg tmux-msg-claude bare", "tmux-msg-claude", "tmux-msg-claude"},
		{"canonical full path", "/usr/local/bin/tmux-tell-claude", ""},
		{"canonical bare", "tmux-tell-claude", ""},
		{"unrelated", "/usr/bin/grep", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			warnIfDeprecatedName(tc.argv0, &stderr)
			got := stderr.String()
			hasWarn := strings.Contains(got, "WARN deprecated_surface_used")
			if hasWarn != (tc.wantName != "") {
				t.Errorf("argv0=%q: warn=%v, want %v (out=%q)", tc.argv0, hasWarn, tc.wantName != "", got)
			}
			if tc.wantName != "" {
				// Names the matched alias + the removal version + the migration
				// pointer so the WARN is actionable + greppable (ADR-0008).
				for _, want := range []string{"name=" + tc.wantName, "removal=v1.0", "tmux-tell-claude"} {
					if !strings.Contains(got, want) {
						t.Errorf("warn missing %q; got %q", want, got)
					}
				}
			}
		})
	}
}

// TestWarnIfDeprecatedEnv pins the env-var deprecation WARNs (#440 Phase 3):
// a set $CLAUDE_MSG_DB / $CLAUDE_MSG_CONFIG warns naming its TMUX_TELL_* successor;
// the new vars alone are silent.
func TestWarnIfDeprecatedEnv(t *testing.T) {
	t.Run("legacy DB var → WARN naming TMUX_TELL_DB", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", "/x.db")
		t.Setenv("CLAUDE_MSG_CONFIG", "")
		var s bytes.Buffer
		warnIfDeprecatedEnv(&s)
		if !strings.Contains(s.String(), "deprecated_env_var_used name=CLAUDE_MSG_DB") ||
			!strings.Contains(s.String(), "TMUX_TELL_DB") {
			t.Errorf("got %q", s.String())
		}
	})
	t.Run("legacy config var → WARN naming TMUX_TELL_CONFIG", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", "")
		t.Setenv("CLAUDE_MSG_CONFIG", "/x.toml")
		var s bytes.Buffer
		warnIfDeprecatedEnv(&s)
		if !strings.Contains(s.String(), "deprecated_env_var_used name=CLAUDE_MSG_CONFIG") ||
			!strings.Contains(s.String(), "TMUX_TELL_CONFIG") {
			t.Errorf("got %q", s.String())
		}
	})
	t.Run("new vars only → silent", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", "")
		t.Setenv("CLAUDE_MSG_CONFIG", "")
		t.Setenv("TMUX_TELL_DB", "/x.db")
		var s bytes.Buffer
		warnIfDeprecatedEnv(&s)
		if s.Len() != 0 {
			t.Errorf("want silent, got %q", s.String())
		}
	})
}

// TestWarnIfLegacyDataPath_DB pins the DB migration WARN: the default resolution
// falling back to the legacy data dir emits a greppable note with the verbatim
// `mv` recipe, suppressed once an explicit DB env override is set. (Only the DB
// branch is asserted — the config branch stats the un-overridable /etc consts,
// whose logic is pinned host-independently in config.TestResolveConfigPath.)
func TestWarnIfLegacyDataPath_DB(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("TMUX_TELL_DB", "")
	t.Setenv("CLAUDE_MSG_DB", "")
	legacy := filepath.Join(data, "tmux-msg", "messages.db")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var s bytes.Buffer
	warnIfLegacyDataPath(&s)
	if !strings.Contains(s.String(), "legacy_data_path_in_use kind=db") ||
		!strings.Contains(s.String(), "mv "+filepath.Join(data, "tmux-msg")) {
		t.Errorf("DB legacy WARN missing/incomplete: %q", s.String())
	}

	// An explicit DB env override means the operator chose a path → no DB note.
	t.Setenv("TMUX_TELL_DB", "/elsewhere.db")
	var s2 bytes.Buffer
	warnIfLegacyDataPath(&s2)
	if strings.Contains(s2.String(), "kind=db") {
		t.Errorf("env override should suppress the DB note; got %q", s2.String())
	}
}
