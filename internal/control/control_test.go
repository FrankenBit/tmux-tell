package control

import (
	"errors"
	"testing"
)

func TestResolve_SelfScope(t *testing.T) {
	cases := map[string]string{
		"compact":               "/compact",
		"rename":                "/rename",
		"cost":                  "/cost",
		"help":                  "/help",
		"mcp-enable-semaphore":  "/mcp enable semaphore",
		"mcp-disable-semaphore": "/mcp disable semaphore",
		"mcp-restart-semaphore": "/mcp restart semaphore",
		"/compact":              "/compact",
		"COMPACT":               "/compact",
		"  cost  ":              "/cost",
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
		"rename":                "/rename",
		"help":                  "/help",
		"mcp-enable-semaphore":  "/mcp enable semaphore",
		"mcp-restart-semaphore": "/mcp restart semaphore",
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
	// mcp-disable-semaphore moved from self+peer to self-only in #28.
	// Pin the regression test so future scope shuffles can't silently
	// expose the peer-DoS surface again.
	for _, in := range []string{"compact", "cost", "mcp-disable-semaphore"} {
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
	want := []string{
		"compact", "cost", "help",
		"mcp-disable-semaphore", "mcp-enable-semaphore", "mcp-restart-semaphore",
		"rename",
	}
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
	if len(self) != len(Allowed) {
		t.Errorf("self names = %v, want all %d commands", self, len(Allowed))
	}
	peer := NamesForScope(ScopePeer)
	want := []string{"help", "mcp-enable-semaphore", "mcp-restart-semaphore", "rename"}
	if len(peer) != len(want) {
		t.Fatalf("peer names = %v, want %v", peer, want)
	}
	for i, n := range peer {
		if n != want[i] {
			t.Fatalf("peer[%d] = %q, want %q", i, n, want[i])
		}
	}
}
