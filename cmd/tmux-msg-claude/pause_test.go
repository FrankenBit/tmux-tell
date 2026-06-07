package main

import (
	"bytes"
	"context"
	"testing"
)

func TestPause_OneAgent(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	var stdout, stderr bytes.Buffer
	exit := runPauseWithStore(context.Background(), s, "alice", true, "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["paused"] != true || got["agent"] != "alice" {
		t.Errorf("got %v", got)
	}
	a, _ := s.GetAgent(context.Background(), "alice")
	if !a.Paused {
		t.Errorf("alice not paused in store")
	}
	b, _ := s.GetAgent(context.Background(), "bob")
	if b.Paused {
		t.Errorf("bob should be unaffected")
	}
}

func TestPause_All_ReturnsPerAgentList(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	var stdout, stderr bytes.Buffer
	exit := runPauseWithStore(context.Background(), s, "", true, "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["paused"] != true {
		t.Errorf("paused flag = %v", got["paused"])
	}
	agents, ok := got["agents"].([]any)
	if !ok {
		t.Fatalf("agents not an array: %v", got["agents"])
	}
	if len(agents) != 3 {
		t.Errorf("agents = %d, want 3", len(agents))
	}
	names := map[string]bool{}
	for _, a := range agents {
		m := a.(map[string]any)
		if m["paused"] != true {
			t.Errorf("agent %v paused = %v", m["name"], m["paused"])
		}
		names[m["name"].(string)] = true
	}
	for _, want := range []string{"alice", "bob", "carol"} {
		if !names[want] {
			t.Errorf("missing agent %q in result", want)
		}
	}
}

func TestPause_All_TextLists(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	var stdout, stderr bytes.Buffer
	_ = runPauseWithStore(context.Background(), s, "", true, "text", &stdout, &stderr)
	out := stdout.String()
	for _, want := range []string{"paused applied to 2 agent(s)", "NAME\tPAUSED", "alice\tyes", "bob\tyes"} {
		if !contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 &&
		stringIndex(haystack, needle) >= 0
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestPause_UnknownAgent(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	var stdout, stderr bytes.Buffer
	exit := runPauseWithStore(context.Background(), s, "ghost", true, "json", &stdout, &stderr)
	if exit != exitUnavailable {
		t.Errorf("exit = %d, want %d", exit, exitUnavailable)
	}
}

func TestResume_FlipsBack(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	_ = s.SetPaused(context.Background(), "alice", true)

	var stdout, stderr bytes.Buffer
	exit := runPauseWithStore(context.Background(), s, "alice", false, "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	a, _ := s.GetAgent(context.Background(), "alice")
	if a.Paused {
		t.Errorf("alice should be unpaused")
	}
}

func TestPauseCLI_MutualExclusion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := runPauseCLI([]string{"--db", ":memory:", "--all", "alice"}, true, &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d", exit, exitUsage)
	}
}

func TestPauseCLI_MissingArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := runPauseCLI([]string{"--db", ":memory:"}, true, &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d", exit, exitUsage)
	}
}
