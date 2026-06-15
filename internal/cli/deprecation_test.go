package cli

import (
	"bytes"
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

func TestWarnIfDeprecatedName(t *testing.T) {
	cases := []struct {
		name     string
		argv0    string
		wantWarn bool
	}{
		{"legacy alias full path", "/usr/local/bin/claude-msg", true},
		{"legacy alias bare", "claude-msg", true},
		{"canonical full path", "/usr/local/bin/tmux-tell-claude", false},
		{"canonical bare", "tmux-tell-claude", false},
		{"unrelated", "/usr/bin/grep", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			warnIfDeprecatedName(tc.argv0, &stderr)
			got := stderr.String()
			hasWarn := strings.Contains(got, "WARN deprecated_surface_used name=claude-msg")
			if hasWarn != tc.wantWarn {
				t.Errorf("argv0=%q: warn=%v, want %v (out=%q)", tc.argv0, hasWarn, tc.wantWarn, got)
			}
			if tc.wantWarn {
				// The removal version + the migration pointer must be present so
				// the WARN is actionable + greppable (ADR-0008 worked example).
				for _, want := range []string{"removal=v1.0", "tmux-tell-claude"} {
					if !strings.Contains(got, want) {
						t.Errorf("warn missing %q; got %q", want, got)
					}
				}
			}
		})
	}
}
