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

func TestPause_All(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	var stdout, stderr bytes.Buffer
	exit := runPauseWithStore(context.Background(), s, "", true, "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["paused"] != true {
		t.Errorf("paused = %v", got["paused"])
	}
	if int(got["updated"].(float64)) != 3 {
		t.Errorf("updated = %v, want 3", got["updated"])
	}
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
