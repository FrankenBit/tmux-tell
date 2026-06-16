package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveConfigPath pins the #440 Phase 3 config precedence: $TMUX_TELL_CONFIG
// > the deprecated $CLAUDE_MSG_CONFIG > DefaultPath, with a lazy fallback to the
// legacy path only when the default is absent but the legacy file exists. legacy
// is reported solely for that default-path fallback (the migration WARN keys on it).
func TestResolveConfigPath(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "tmux-tell", "config.toml")
	legacy := filepath.Join(dir, "tmux-msg", "config.toml")
	write := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("new env wins, never legacy", func(t *testing.T) {
		got, lg := resolveConfigPath("/new.toml", "/old.toml", def, legacy)
		if got != "/new.toml" || lg {
			t.Errorf("got (%q,%v), want (/new.toml,false)", got, lg)
		}
	})
	t.Run("legacy env used when new unset, never legacy-flagged", func(t *testing.T) {
		got, lg := resolveConfigPath("", "/old.toml", def, legacy)
		if got != "/old.toml" || lg {
			t.Errorf("got (%q,%v), want (/old.toml,false)", got, lg)
		}
	})
	t.Run("no env, neither file → default", func(t *testing.T) {
		got, lg := resolveConfigPath("", "", def, legacy)
		if got != def || lg {
			t.Errorf("got (%q,%v), want (%q,false)", got, lg, def)
		}
	})
	t.Run("no env, only legacy file → legacy fallback flagged", func(t *testing.T) {
		write(legacy)
		got, lg := resolveConfigPath("", "", def, legacy)
		if got != legacy || !lg {
			t.Errorf("got (%q,%v), want (%q,true)", got, lg, legacy)
		}
	})
	t.Run("no env, default exists → default wins over lingering legacy", func(t *testing.T) {
		write(def) // legacy still on disk from the prior subtest's dir? use a fresh check
		got, lg := resolveConfigPath("", "", def, legacy)
		if got != def || lg {
			t.Errorf("got (%q,%v), want (%q,false)", got, lg, def)
		}
	})
}
