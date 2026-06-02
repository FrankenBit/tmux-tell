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
		// Self scope passes the same name for sender + recipient.
		// PeerEdges is not consulted in self scope, so the names
		// can be anything matching the caller's actual identity.
		got, err := Resolve(in, ScopeSelf, "alice", "alice")
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
		// Globally peer-allowed commands resolve for any (sender,
		// recipient) pair — pick arbitrary distinct names to make
		// that explicit.
		got, err := Resolve(in, ScopePeer, "alice", "bob")
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
	// expose the peer-DoS surface again. clear is omitted here because
	// its peer-denial is conditional (lifted for the Bosun→Pilot edge
	// per #60) — see TestResolve_PeerScope_EdgeRule for that pinning.
	for _, in := range []string{"compact", "cost", "mcp-disable-semaphore"} {
		_, err := Resolve(in, ScopePeer, "alice", "bob")
		if !errors.Is(err, ErrScopeDenied) {
			t.Errorf("Resolve(%q, peer): want ErrScopeDenied, got %v", in, err)
		}
	}
}

func TestResolve_RejectsUnknown(t *testing.T) {
	// clear / /clear used to live here as "must stay unknown" but #60
	// added them to the whitelist with the PeerEdges exception layer.
	// bash and rm remain unknown — they are not whitelisted at all.
	for _, scope := range []Scope{ScopeSelf, ScopePeer} {
		for _, in := range []string{"", "/", "bash", "rm"} {
			_, err := Resolve(in, scope, "alice", "bob")
			if !errors.Is(err, ErrNotAllowed) {
				t.Errorf("Resolve(%q, %s): want ErrNotAllowed, got %v", in, scope, err)
			}
		}
	}
}

// TestResolve_PeerScope_EdgeRule pins the per-edge exception layer
// added in #60: /clear is globally denied for peer scope, but a
// matching (sender, recipient) entry in PeerEdges grants the
// invocation narrowly. Verifies:
//
//  1. positive: Bosun→Pilot/clear returns the literal text
//  2. negative: any sender other than Bosun (engineer→pilot)
//  3. negative: any recipient other than Pilot (bosun→engineer)
//  4. negative: self scope (bosun→bosun) — clear's Self flag is false,
//     and PeerEdges only applies in peer scope, so the ErrScopeDenied
//     comes from the self-flag check rather than the edge lookup
//
// The negative cases protect against accidentally widening the
// exception when refactoring the lookup logic (e.g. flipping From/To
// or using prefix-match instead of exact-match).
func TestResolve_PeerScope_EdgeRule(t *testing.T) {
	got, err := Resolve("clear", ScopePeer, "bosun", "pilot")
	if err != nil {
		t.Errorf("Resolve(clear, peer, bosun→pilot): want /clear, got error %v", err)
	}
	if got != "/clear" {
		t.Errorf("Resolve(clear, peer, bosun→pilot) = %q, want /clear", got)
	}

	// Negative: wrong sender — Engineer is not on the clear edge.
	if _, err := Resolve("clear", ScopePeer, "engineer", "pilot"); !errors.Is(err, ErrScopeDenied) {
		t.Errorf("Resolve(clear, peer, engineer→pilot): want ErrScopeDenied, got %v", err)
	}
	// Negative: wrong recipient — Engineer is not the clear target.
	if _, err := Resolve("clear", ScopePeer, "bosun", "engineer"); !errors.Is(err, ErrScopeDenied) {
		t.Errorf("Resolve(clear, peer, bosun→engineer): want ErrScopeDenied, got %v", err)
	}
	// Negative: edge direction matters — reversed (pilot→bosun) is denied.
	if _, err := Resolve("clear", ScopePeer, "pilot", "bosun"); !errors.Is(err, ErrScopeDenied) {
		t.Errorf("Resolve(clear, peer, pilot→bosun): want ErrScopeDenied, got %v", err)
	}
	// Negative: self scope — clear has Self=false, and PeerEdges is
	// not consulted in self scope.
	if _, err := Resolve("clear", ScopeSelf, "bosun", "bosun"); !errors.Is(err, ErrScopeDenied) {
		t.Errorf("Resolve(clear, self, bosun→bosun): want ErrScopeDenied, got %v", err)
	}
}

// TestResolve_SelfScope_ErrorWording pins the three-branch
// differentiation introduced in Surveyor's S1 absorb on PR #61.
// Pre-#60 every Self=false entry also had Peer=true, so the historical
// "is peer-only" wording was always accurate. /clear is the first
// entry to break that invariant (Self=false AND Peer=false). The
// switch in Resolve's ScopeSelf branch now picks the wording based on
// what WOULD have let the caller through:
//
//  1. cmd.Peer == true → "is peer-only" (genuine peer-only, e.g. a
//     hypothetical peer-only command — none today but the wording
//     stays accurate when one lands).
//  2. cmd.Peer == false AND PeerEdges[n] non-empty → "is restricted
//     to specific peer (sender, recipient) edges" — the /clear shape.
//  3. cmd.Peer == false AND PeerEdges[n] empty → "is not invokable in
//     any scope" — theoretical entry that exists in Allowed but is
//     denied everywhere; no command has this shape today but the
//     branch covers it for completeness.
func TestResolve_SelfScope_ErrorWording(t *testing.T) {
	// (2) /clear with self scope hits the edge-restricted wording.
	_, err := Resolve("clear", ScopeSelf, "bosun", "bosun")
	if err == nil {
		t.Fatal("Resolve(clear, self): want error, got nil")
	}
	if !errors.Is(err, ErrScopeDenied) {
		t.Errorf("Resolve(clear, self): want ErrScopeDenied wrapping, got %v", err)
	}
	want := "is restricted to specific peer (sender, recipient) edges; not self-invokable"
	if !contains(err.Error(), want) {
		t.Errorf("Resolve(clear, self) wording = %q, want substring %q", err.Error(), want)
	}
	notWant := "is peer-only"
	if contains(err.Error(), notWant) {
		t.Errorf("Resolve(clear, self) wording = %q, must NOT contain %q (the pre-#60 wording was wrong for this shape)", err.Error(), notWant)
	}
}

// contains is a tiny helper to avoid importing strings just for one
// test substring check. (The package's other tests don't use strings
// either.)
func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestNames_SortedAndComplete(t *testing.T) {
	names := Names()
	want := []string{
		"clear", "compact", "cost", "help",
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
	// Self scope: every Allowed entry whose Self flag is true. clear
	// is excluded because its Self flag is false (#60).
	self := NamesForScope(ScopeSelf, "alice", "alice")
	wantSelf := []string{
		"compact", "cost", "help",
		"mcp-disable-semaphore", "mcp-enable-semaphore", "mcp-restart-semaphore",
		"rename",
	}
	if len(self) != len(wantSelf) {
		t.Fatalf("self names = %v, want %v", self, wantSelf)
	}
	for i, n := range self {
		if n != wantSelf[i] {
			t.Fatalf("self[%d] = %q, want %q", i, n, wantSelf[i])
		}
	}

	// Peer scope, no edge match: only globally peer-allowed commands.
	peer := NamesForScope(ScopePeer, "alice", "bob")
	wantPeer := []string{"help", "mcp-enable-semaphore", "mcp-restart-semaphore", "rename"}
	if len(peer) != len(wantPeer) {
		t.Fatalf("peer names = %v, want %v", peer, wantPeer)
	}
	for i, n := range peer {
		if n != wantPeer[i] {
			t.Fatalf("peer[%d] = %q, want %q", i, n, wantPeer[i])
		}
	}

	// Peer scope WITH edge match: bosun→pilot also surfaces "clear".
	// Pins that error-message context stays accurate for edge-enabled
	// callers — without this, a Bosun trying /clear on Pilot would
	// see "peer-invokable: [help …]" and think clear isn't available.
	bosunPilot := NamesForScope(ScopePeer, "bosun", "pilot")
	wantBosunPilot := []string{"clear", "help", "mcp-enable-semaphore", "mcp-restart-semaphore", "rename"}
	if len(bosunPilot) != len(wantBosunPilot) {
		t.Fatalf("bosun→pilot names = %v, want %v", bosunPilot, wantBosunPilot)
	}
	for i, n := range bosunPilot {
		if n != wantBosunPilot[i] {
			t.Fatalf("bosun→pilot[%d] = %q, want %q", i, n, wantBosunPilot[i])
		}
	}
}
