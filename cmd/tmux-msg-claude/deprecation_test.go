package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWarnIfDeprecatedName(t *testing.T) {
	cases := []struct {
		name     string
		argv0    string
		wantWarn bool
	}{
		{"legacy alias full path", "/usr/local/bin/claude-msg", true},
		{"legacy alias bare", "claude-msg", true},
		{"canonical full path", "/usr/local/bin/tmux-msg-claude", false},
		{"canonical bare", "tmux-msg-claude", false},
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
				for _, want := range []string{"removal=v0.11.0", "tmux-msg-claude"} {
					if !strings.Contains(got, want) {
						t.Errorf("warn missing %q; got %q", want, got)
					}
				}
			}
		})
	}
}
