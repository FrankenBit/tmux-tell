package cli

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// TestServeCLI_LogsDBPathOnStartup verifies that `tmux-tell-claude serve`
// emits the claude_msg_db startup log on stderr with the correct source label
// for each DB-resolution path (#290). The log fires before store.Open so it
// is visible even when the open would fail (e.g. the default path doesn't
// exist on a dev machine).
func TestServeCLI_LogsDBPathOnStartup(t *testing.T) {
	db := filepath.Join(t.TempDir(), "messages.db")

	t.Run("flag", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", "")
		var stderr strings.Builder
		// Nonexistent agent exits quickly (exitUnavailable); the log fires before that.
		runServeCLI([]string{"--db", db, "--agent", "nope"}, io.Discard, &stderr)
		if !strings.Contains(stderr.String(), "claude_msg_db="+db) {
			t.Errorf("missing claude_msg_db path; stderr=%s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "source=flag(--db)") {
			t.Errorf("missing source label; stderr=%s", stderr.String())
		}
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", db)
		var stderr strings.Builder
		runServeCLI([]string{"--agent", "nope"}, io.Discard, &stderr)
		if !strings.Contains(stderr.String(), "source=env(CLAUDE_MSG_DB)") {
			t.Errorf("missing env source label; stderr=%s", stderr.String())
		}
	})
}

// TestMCPCLI_LogsDBPathOnStartup verifies that `tmux-tell-claude mcp` emits
// the claude_msg_db startup log on stderr (#290). runMCPCLI serves until EOF;
// an empty reader returns immediately after the log fires.
func TestMCPCLI_LogsDBPathOnStartup(t *testing.T) {
	db := filepath.Join(t.TempDir(), "messages.db")

	t.Run("flag", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", "")
		var stderr strings.Builder
		runMCPCLI([]string{"--db", db}, strings.NewReader(""), io.Discard, &stderr)
		if !strings.Contains(stderr.String(), "claude_msg_db="+db) {
			t.Errorf("missing claude_msg_db path; stderr=%s", stderr.String())
		}
		if !strings.Contains(stderr.String(), "source=flag(--db)") {
			t.Errorf("missing source label; stderr=%s", stderr.String())
		}
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", db)
		var stderr strings.Builder
		runMCPCLI(nil, strings.NewReader(""), io.Discard, &stderr)
		if !strings.Contains(stderr.String(), "source=env(CLAUDE_MSG_DB)") {
			t.Errorf("missing env source label; stderr=%s", stderr.String())
		}
	})

	t.Run("default", func(t *testing.T) {
		t.Setenv("CLAUDE_MSG_DB", "")
		// Isolate the user-home default (#308) to a temp HOME so the resolved
		// default DB lands in a throwaway dir rather than polluting the test
		// runner's real ~/.local/share. XDG_DATA_HOME cleared so the $HOME
		// fallback branch is exercised. The assertion only cares about the
		// source label, which the startup log fires before store.Open.
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", t.TempDir())
		var stderr strings.Builder
		runMCPCLI(nil, strings.NewReader(""), io.Discard, &stderr)
		if !strings.Contains(stderr.String(), "source=default(env unset)") {
			t.Errorf("missing default source label; stderr=%s", stderr.String())
		}
	})
}
