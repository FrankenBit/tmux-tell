package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultDBLocation_XDG pins the #308 user-home DB resolution: the default
// path follows the XDG Base Directory spec — $XDG_DATA_HOME when set, else
// $HOME/.local/share, with a relative fallback when neither is set. The subdir
// is the canonical tmux-tell name (#440 Phase 3); a non-existent path resolves
// to it (no legacy file → no fallback).
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
			wantPath: filepath.Join("/custom/data", "tmux-tell", "messages.db"),
		},
		{
			// A nonexistent home keeps this a pure path-construction check — the
			// #440 lazy fallback only triggers when a legacy file is on disk
			// (covered by TestDefaultDBLocation_LazyMigration).
			name:     "XDG unset falls back to HOME/.local/share",
			xdg:      "",
			home:     "/home/nonexistent-test-user",
			wantPath: filepath.Join("/home/nonexistent-test-user", ".local", "share", "tmux-tell", "messages.db"),
		},
		{
			name:     "both unset falls back to relative .local/share",
			xdg:      "",
			home:     "",
			wantPath: filepath.Join(".local", "share", "tmux-tell", "messages.db"),
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

// TestDefaultDBLocation_LazyMigration pins the #440 Phase 3 lazy fallback: an
// in-place operator who upgraded but hasn't moved their data keeps resolving to
// the legacy tmux-msg path until the tmux-tell path exists; a fresh install (no
// legacy file) lands on tmux-tell.
func TestDefaultDBLocation_LazyMigration(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	newPath := filepath.Join(data, "tmux-tell", "messages.db")
	legacyPath := filepath.Join(data, "tmux-msg", "messages.db")

	// Nothing on disk → fresh install → tmux-tell, not legacy.
	if got, legacy := defaultDBLocationResolved(); got != newPath || legacy {
		t.Fatalf("fresh: got (%q, %v), want (%q, false)", got, legacy, newPath)
	}

	// Only the legacy DB exists → fall back to it + report legacy=true.
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, legacy := defaultDBLocationResolved(); got != legacyPath || !legacy {
		t.Fatalf("legacy-only: got (%q, %v), want (%q, true)", got, legacy, legacyPath)
	}

	// Once the tmux-tell DB exists, it wins even if the legacy one lingers.
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, legacy := defaultDBLocationResolved(); got != newPath || legacy {
		t.Fatalf("both-exist: got (%q, %v), want (%q, false)", got, legacy, newPath)
	}
}

// TestResolveDBPath_Precedence pins the resolution precedence: explicit --db flag
// > $TMUX_TELL_DB > the deprecated $CLAUDE_MSG_DB > the user-home default (#308 /
// #440 Phase 3). The deprecated env var still resolves (legacy works through v1.0).
func TestResolveDBPath_Precedence(t *testing.T) {
	t.Run("flag wins over env and default", func(t *testing.T) {
		t.Setenv("TMUX_TELL_DB", "/from/newenv.db")
		t.Setenv("CLAUDE_MSG_DB", "/from/env.db")
		t.Setenv("XDG_DATA_HOME", "/custom/data")
		if got := resolveDBPath("/from/flag.db"); got != "/from/flag.db" {
			t.Errorf("resolveDBPath = %q, want the flag value", got)
		}
	})

	t.Run("new env wins over legacy env", func(t *testing.T) {
		t.Setenv("TMUX_TELL_DB", "/from/newenv.db")
		t.Setenv("CLAUDE_MSG_DB", "/from/env.db")
		if got := resolveDBPath(""); got != "/from/newenv.db" {
			t.Errorf("resolveDBPath = %q, want $TMUX_TELL_DB to win over $CLAUDE_MSG_DB", got)
		}
	})

	t.Run("legacy env still resolves when new env unset", func(t *testing.T) {
		t.Setenv("TMUX_TELL_DB", "")
		t.Setenv("CLAUDE_MSG_DB", "/from/env.db")
		t.Setenv("XDG_DATA_HOME", "/custom/data")
		if got := resolveDBPath(""); got != "/from/env.db" {
			t.Errorf("resolveDBPath = %q, want the deprecated $CLAUDE_MSG_DB fallback", got)
		}
	})

	t.Run("default resolves to user-home XDG path when no override", func(t *testing.T) {
		t.Setenv("TMUX_TELL_DB", "")
		t.Setenv("CLAUDE_MSG_DB", "")
		t.Setenv("XDG_DATA_HOME", "/custom/data")
		t.Setenv("HOME", "/home/alex")
		want := filepath.Join("/custom/data", "tmux-tell", "messages.db")
		if got := resolveDBPath(""); got != want {
			t.Errorf("resolveDBPath = %q, want the user-home default %q", got, want)
		}
	})
}
