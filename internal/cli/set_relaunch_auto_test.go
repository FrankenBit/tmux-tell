package cli

import (
	"context"
	"errors"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

func TestSetRelaunchCmd_Core(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // best-effort close in test
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "pilot", "%6"); err != nil {
		t.Fatal(err)
	}

	const cmd = "chamber-claude.sh Pilot"
	res, err := setRelaunchCmd(ctx, s, "pilot", cmd)
	if err != nil {
		t.Fatalf("setRelaunchCmd: %v", err)
	}
	if !res.OK || res.Agent != "pilot" || res.RelaunchCmd != cmd {
		t.Errorf("result = %+v, want {OK:true Agent:pilot RelaunchCmd:%q}", res, cmd)
	}
	a, _ := s.GetAgent(ctx, "pilot")
	if a.RelaunchCmd != cmd {
		t.Errorf("persisted relaunch_cmd = %q, want %q", a.RelaunchCmd, cmd)
	}

	// Empty target is rejected at the surface (before the store).
	if _, err := setRelaunchCmd(ctx, s, "", cmd); err == nil {
		t.Error("empty target accepted, want an error")
	}
	// A missing target surfaces ErrNotFound.
	if _, err := setRelaunchCmd(ctx, s, "ghost", cmd); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSetAutoRestart_Core(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // best-effort close in test
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "pilot", "%6"); err != nil {
		t.Fatal(err)
	}

	res, err := setAutoRestart(ctx, s, "pilot", true)
	if err != nil {
		t.Fatalf("setAutoRestart: %v", err)
	}
	if !res.OK || res.Agent != "pilot" || !res.AutoRestart {
		t.Errorf("result = %+v, want {OK:true Agent:pilot AutoRestart:true}", res)
	}
	a, _ := s.GetAgent(ctx, "pilot")
	if !a.AutoRestart {
		t.Errorf("persisted auto_restart = %v, want true", a.AutoRestart)
	}

	if _, err := setAutoRestart(ctx, s, "", true); err == nil {
		t.Error("empty target accepted, want an error")
	}
	if _, err := setAutoRestart(ctx, s, "ghost", true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// parseOnOff accepts the on/off vocabulary (case-insensitive) and rejects
// anything else — the mutation anchor guarding against a silently-misparsed flag
// value flipping the chamber's auto-restart the wrong way.
func TestParseOnOff(t *testing.T) {
	on := []string{"on", "ON", "true", "1", "yes", "enable", "enabled", " on "}
	off := []string{"off", "OFF", "false", "0", "no", "disable", "disabled"}
	bad := []string{"", "maybe", "2", "onoff"}
	for _, v := range on {
		if got, err := parseOnOff(v); err != nil || !got {
			t.Errorf("parseOnOff(%q) = %v, %v; want true, nil", v, got, err)
		}
	}
	for _, v := range off {
		if got, err := parseOnOff(v); err != nil || got {
			t.Errorf("parseOnOff(%q) = %v, %v; want false, nil", v, got, err)
		}
	}
	for _, v := range bad {
		if _, err := parseOnOff(v); err == nil {
			t.Errorf("parseOnOff(%q) err = nil, want an error", v)
		}
	}
}
