package store

import (
	"context"
	"errors"
	"testing"
)

// A freshly-registered agent has relaunch_cmd = "" and auto_restart = false (the
// migration defaults). SetRelaunchCmd / SetAutoRestart round-trip through both
// GetAgent and ListAgents. Defaults matter: merging #285/#730 must not change any
// live chamber's behaviour until the operator opts in.
func TestSetRelaunchAndAutoRestart_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "pilot", "%6"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	a, err := s.GetAgent(ctx, "pilot")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.RelaunchCmd != "" || a.AutoRestart {
		t.Errorf("fresh agent relaunch_cmd=%q auto_restart=%v, want \"\" / false", a.RelaunchCmd, a.AutoRestart)
	}

	const cmd = "claude --resume Pilot"
	if err := s.SetRelaunchCmd(ctx, "pilot", cmd); err != nil {
		t.Fatalf("SetRelaunchCmd: %v", err)
	}
	if err := s.SetAutoRestart(ctx, "pilot", true); err != nil {
		t.Fatalf("SetAutoRestart: %v", err)
	}

	a, _ = s.GetAgent(ctx, "pilot")
	if a.RelaunchCmd != cmd || !a.AutoRestart {
		t.Errorf("GetAgent relaunch_cmd=%q auto_restart=%v, want %q / true", a.RelaunchCmd, a.AutoRestart, cmd)
	}

	// The listing path carries both too.
	list, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, ag := range list {
		if ag.Name == "pilot" {
			found = true
			if ag.RelaunchCmd != cmd || !ag.AutoRestart {
				t.Errorf("ListAgents relaunch_cmd=%q auto_restart=%v, want %q / true", ag.RelaunchCmd, ag.AutoRestart, cmd)
			}
		}
	}
	if !found {
		t.Fatal("pilot not in ListAgents")
	}

	// Both are reversible: empty cmd unconfigures, false disables.
	if err := s.SetRelaunchCmd(ctx, "pilot", ""); err != nil {
		t.Fatalf("SetRelaunchCmd(\"\"): %v", err)
	}
	if err := s.SetAutoRestart(ctx, "pilot", false); err != nil {
		t.Fatalf("SetAutoRestart(false): %v", err)
	}
	a, _ = s.GetAgent(ctx, "pilot")
	if a.RelaunchCmd != "" || a.AutoRestart {
		t.Errorf("after clear: relaunch_cmd=%q auto_restart=%v, want \"\" / false", a.RelaunchCmd, a.AutoRestart)
	}
}

func TestSetRelaunchAndAutoRestart_UnknownAgent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SetRelaunchCmd(ctx, "ghost", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetRelaunchCmd err = %v, want ErrNotFound", err)
	}
	if err := s.SetAutoRestart(ctx, "ghost", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetAutoRestart err = %v, want ErrNotFound", err)
	}
}
