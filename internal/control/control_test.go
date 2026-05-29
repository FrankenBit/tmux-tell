package control

import (
	"errors"
	"testing"
)

func TestResolve_AllowsWhitelistedCommands(t *testing.T) {
	cases := map[string]string{
		"compact":  "/compact",
		"rename":   "/rename",
		"cost":     "/cost",
		"help":     "/help",
		"/compact": "/compact",
		"COMPACT":  "/compact",
		"  cost  ": "/cost",
	}
	for in, want := range cases {
		got, err := Resolve(in)
		if err != nil {
			t.Errorf("Resolve(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolve_RejectsUnknown(t *testing.T) {
	for _, in := range []string{"", "/", "clear", "bash", "rm", "/clear"} {
		_, err := Resolve(in)
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Resolve(%q): want ErrNotAllowed, got %v", in, err)
		}
	}
}

func TestNames_SortedAndComplete(t *testing.T) {
	names := Names()
	want := []string{"compact", "cost", "help", "rename"}
	if len(names) != len(want) {
		t.Fatalf("Names() = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Fatalf("Names()[%d] = %q, want %q", i, n, want[i])
		}
	}
}
