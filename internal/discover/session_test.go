package discover

import (
	"context"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestLookupBySessionID: the primary exact-match resolution path (#626 Phase
// 1b). Each pane's process carries a CLAUDE_CODE_SESSION_ID; lookup resolves
// the pane hosting a given session UUID, and nothing else.
func TestLookupBySessionID(t *testing.T) {
	prev := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%1\t100\tBosun\tclaude\n" +
			"%5\t200\tPilot\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prev) })

	w := &Walker{
		ChildrenReader: func(int) []int { return nil },
		EnvironReader: func(pid int, key string) (string, bool) {
			if key != ClaudeSessionIDEnv {
				return "", false
			}
			switch pid {
			case 100:
				return "AAA-uuid", true
			case 200:
				return "BBB-uuid", true
			}
			return "", false
		},
		MaxDepth: 3,
	}

	got, err := w.LookupBySessionID(context.Background(), "BBB-uuid")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "%5" {
		t.Errorf("got %q, want %%5", got)
	}
	if got, _ := w.LookupBySessionID(context.Background(), "ghost-uuid"); got != "" {
		t.Errorf("unknown session should resolve nowhere, got %q", got)
	}
	if got, _ := w.LookupBySessionID(context.Background(), ""); got != "" {
		t.Errorf("empty session-id should resolve nowhere, got %q", got)
	}
}

// TestSessionIDForPane: register-time self-discovery of a single pane's
// session UUID.
func TestSessionIDForPane(t *testing.T) {
	prev := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%1\t100\tBosun\tclaude\n" +
			"%5\t200\tPilot\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prev) })

	w := &Walker{
		ChildrenReader: func(int) []int { return nil },
		EnvironReader: func(pid int, key string) (string, bool) {
			if key == ClaudeSessionIDEnv && pid == 200 {
				return "BBB-uuid", true
			}
			return "", false
		},
		MaxDepth: 3,
	}

	if got, ok := w.SessionIDForPane(context.Background(), "%5"); !ok || got != "BBB-uuid" {
		t.Errorf("SessionIDForPane(%%5) = %q,%v; want BBB-uuid,true", got, ok)
	}
	if got, ok := w.SessionIDForPane(context.Background(), "%1"); ok || got != "" {
		t.Errorf("SessionIDForPane(%%1) = %q,%v; want \"\",false (no session env)", got, ok)
	}
	if got, ok := w.SessionIDForPane(context.Background(), "%99"); ok || got != "" {
		t.Errorf("SessionIDForPane(%%99) = %q,%v; want \"\",false (no such pane)", got, ok)
	}
}

// TestSessionIDForPane_DescendantWalk: the session env lives on a CHILD of the
// pane's root process (bash -> claude), so the walk must descend to find it.
func TestSessionIDForPane_DescendantWalk(t *testing.T) {
	prev := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%7\t700\tbash\tbash\n"), nil // pane root is bash; claude is a child
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prev) })

	w := &Walker{
		ChildrenReader: func(pid int) []int {
			if pid == 700 {
				return []int{701} // bash -> claude
			}
			return nil
		},
		EnvironReader: func(pid int, key string) (string, bool) {
			if key == ClaudeSessionIDEnv && pid == 701 {
				return "CHILD-uuid", true
			}
			return "", false
		},
		MaxDepth: 3,
	}
	if got, ok := w.SessionIDForPane(context.Background(), "%7"); !ok || got != "CHILD-uuid" {
		t.Errorf("descendant session-id = %q,%v; want CHILD-uuid,true", got, ok)
	}
}

// TestSessionID_NilEnvironReader: a Walker built without an EnvironReader (the
// existing-caller shape, e.g. the Phase-1a serve tests) disables session-id
// discovery — nothing matches, no panic.
func TestSessionID_NilEnvironReader(t *testing.T) {
	prev := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%1\t100\tBosun\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prev) })

	w := &Walker{
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       3,
		// EnvironReader intentionally nil
	}
	if got, _ := w.LookupBySessionID(context.Background(), "AAA-uuid"); got != "" {
		t.Errorf("nil EnvironReader should resolve nowhere, got %q", got)
	}
	if got, ok := w.SessionIDForPane(context.Background(), "%1"); ok || got != "" {
		t.Errorf("nil EnvironReader SessionIDForPane = %q,%v; want \"\",false", got, ok)
	}
}
