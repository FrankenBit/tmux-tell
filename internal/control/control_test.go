package control

import (
	"errors"
	"testing"
)

func TestResolve_SelfScope(t *testing.T) {
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
		got, err := Resolve(in, ScopeSelf)
		if err != nil {
			t.Errorf("Resolve(%q, self): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q, self) = %q, want %q", in, got, want)
		}
	}
}

func TestResolve_PeerScope_OnlyPeerAllowed(t *testing.T) {
	allowed := map[string]string{
		"rename": "/rename",
		"help":   "/help",
	}
	for in, want := range allowed {
		got, err := Resolve(in, ScopePeer)
		if err != nil {
			t.Errorf("Resolve(%q, peer): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q, peer) = %q, want %q", in, got, want)
		}
	}
}

func TestResolve_PeerScope_RejectsSelfOnly(t *testing.T) {
	for _, in := range []string{"compact", "cost"} {
		_, err := Resolve(in, ScopePeer)
		if !errors.Is(err, ErrScopeDenied) {
			t.Errorf("Resolve(%q, peer): want ErrScopeDenied, got %v", in, err)
		}
	}
}

func TestResolve_RejectsUnknown(t *testing.T) {
	for _, scope := range []Scope{ScopeSelf, ScopePeer} {
		for _, in := range []string{"", "/", "clear", "bash", "rm", "/clear"} {
			_, err := Resolve(in, scope)
			if !errors.Is(err, ErrNotAllowed) {
				t.Errorf("Resolve(%q, %s): want ErrNotAllowed, got %v", in, scope, err)
			}
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

func TestNamesForScope(t *testing.T) {
	self := NamesForScope(ScopeSelf)
	if len(self) != 4 {
		t.Errorf("self names = %v, want all 4 commands", self)
	}
	peer := NamesForScope(ScopePeer)
	want := []string{"help", "rename"}
	if len(peer) != len(want) {
		t.Fatalf("peer names = %v, want %v", peer, want)
	}
	for i, n := range peer {
		if n != want[i] {
			t.Fatalf("peer[%d] = %q, want %q", i, n, want[i])
		}
	}
}
