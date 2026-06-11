package cli

import (
	"path/filepath"
	"testing"
)

// TestDefaultDBLocation_XDG pins the #308 user-home DB resolution: the default
// path follows the XDG Base Directory spec — $XDG_DATA_HOME when set, else
// $HOME/.local/share, with a relative fallback when neither is set.
func TestDefaultDBLocation_XDG(t *testing.T) {
	cases := []struct {
		name     string
		xdg      string
		home     string
		wantPath string
	}{
		{
			name:     "XDG_DATA_HOME set wins",
			xdg:      "/custom/data",
			home:     "/home/alex",
			wantPath: filepath.Join("/custom/data", "tmux-msg", "messages.db"),
		},
		{
			name:     "XDG unset falls back to HOME/.local/share",
			xdg:      "",
			home:     "/home/alex",
			wantPath: filepath.Join("/home/alex", ".local", "share", "tmux-msg", "messages.db"),
		},
		{
			name:     "both unset falls back to relative .local/share",
			xdg:      "",
			home:     "",
			wantPath: filepath.Join(".local", "share", "tmux-msg", "messages.db"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", tc.xdg)
			t.Setenv("HOME", tc.home)
			if got := defaultDBLocation(); got != tc.wantPath {
				t.Errorf("defaultDBLocation() = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

// TestResolveDBPath_Precedence pins the resolution precedence is unchanged by
// #308: explicit --db flag > $CLAUDE_MSG_DB > the user-home default. Only the
// default leg moved (no longer /var/lib); the override legs behave as before.
func TestResolveDBPath_Precedence(t *testing.T) {
	t.Run("flag wins over env and default", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", "/from/env.db")
		t.Setenv("XDG_DATA_HOME", "/custom/data")
		if got := resolveDBPath("/from/flag.db"); got != "/from/flag.db" {
			t.Errorf("resolveDBPath = %q, want the flag value", got)
		}
	})

	t.Run("env wins over default", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", "/from/env.db")
		t.Setenv("XDG_DATA_HOME", "/custom/data")
		if got := resolveDBPath(""); got != "/from/env.db" {
			t.Errorf("resolveDBPath = %q, want the env value", got)
		}
	})

	t.Run("default resolves to user-home XDG path when no override", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", "")
		t.Setenv("XDG_DATA_HOME", "/custom/data")
		t.Setenv("HOME", "/home/alex")
		want := filepath.Join("/custom/data", "tmux-msg", "messages.db")
		if got := resolveDBPath(""); got != want {
			t.Errorf("resolveDBPath = %q, want the user-home default %q", got, want)
		}
	})
}
