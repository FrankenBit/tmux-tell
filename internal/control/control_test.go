package control

import (
	"errors"
	"testing"
)

// TestCanonicalize_DeprecatedAliases pins #480's backward-compat: the pre-rename
// mcp-*-tmux-msg macro identifiers canonicalize to their tmux-tell forms (flagged
// as aliases), canonical names pass through unflagged, and Resolve via a legacy
// alias yields the same Text as the canonical name.
func TestCanonicalize_DeprecatedAliases(t *testing.T) {
	aliases := map[string]string{
		"mcp-restart-tmux-msg":  "mcp-restart-tmux-tell",
		"mcp-disable-tmux-msg":  "mcp-disable-tmux-tell",
		"mcp-enable-tmux-msg":   "mcp-enable-tmux-tell",
		"/MCP-Restart-Tmux-Msg": "mcp-restart-tmux-tell", // trim/slash/lowercase too
		"sleep":                 "compact",               // #646 sleep→compact bus-verb rename
		"/SLEEP":                "compact",               // trim/slash/lowercase on the new alias too
	}
	for in, want := range aliases {
		got, wasAlias := Canonicalize(in)
		if got != want || !wasAlias {
			t.Errorf("Canonicalize(%q) = (%q, %v), want (%q, true)", in, got, wasAlias, want)
		}
	}
	for _, canon := range []string{"mcp-restart-tmux-tell", "compact", "rename"} {
		if got, wasAlias := Canonicalize(canon); got != canon || wasAlias {
			t.Errorf("Canonicalize(%q) = (%q, %v), want (%q, false)", canon, got, wasAlias, canon)
		}
	}
	legacy, lerr := Resolve("mcp-restart-tmux-msg", ScopePeer, "a", "b")
	canon, cerr := Resolve("mcp-restart-tmux-tell", ScopePeer, "a", "b")
	if lerr != nil || cerr != nil || legacy != canon || legacy == "" {
		t.Errorf("alias Resolve %q (err %v) != canonical Resolve %q (err %v)", legacy, lerr, canon, cerr)
	}
	// #646: the deprecated `sleep` alias resolves to the same Text as canonical
	// `compact` — both emit the unchanged /compact CLI primitive (self-scope, the
	// only scope where this self-only macro is valid).
	aliasText, aerr := Resolve("sleep", ScopeSelf, "alice", "alice")
	canonText, cerr2 := Resolve("compact", ScopeSelf, "alice", "alice")
	if aerr != nil || cerr2 != nil || aliasText != canonText || aliasText != "/compact" {
		t.Errorf("sleep-alias Resolve %q (err %v) != compact Resolve %q (err %v), want /compact",
			aliasText, aerr, canonText, cerr2)
	}
}

func TestResolve_SelfScope(t *testing.T) {
	cases := map[string]string{
		"compact":               "/compact",
		"rename":                "/rename",
		"cost":                  "/cost",
		"help":                  "/help",
		"mcp-enable-tmux-tell":  "/mcp enable tmux-tell",
		"mcp-disable-tmux-tell": "/mcp disable tmux-tell",
		"mcp-restart-tmux-tell": "/mcp restart tmux-tell",
		"/compact":              "/compact",
		"COMPACT":               "/compact",
		"sleep":                 "/compact", // #646 deprecated alias still resolves
		"/sleep":                "/compact", // alias, trim/slash-normalised
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
		"mcp-enable-tmux-tell":  "/mcp enable tmux-tell",
		"mcp-restart-tmux-tell": "/mcp restart tmux-tell",
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
	// mcp-disable-tmux-tell moved from self+peer to self-only in #28.
	// Pin the regression test so future scope shuffles can't silently
	// expose the peer-DoS surface again. clear is omitted here because
	// its peer-denial is conditional (lifted for the Bosun→Pilot edge
	// per #60) — see TestResolve_PeerScope_EdgeRule for that pinning.
	for _, in := range []string{"compact", "cost", "mcp-disable-tmux-tell"} {
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

// TestResolve_PeerScope_EdgeRule_Quartermaster pins the Quartermaster→
// Pilot /clear edge added in #167, mirroring the Bosun→Pilot edge.
// Quartermaster is an established dispatcher into Pilot's clear-before-
// each-task lifecycle (feedback_pilot_clear_before_each_task), so it gets
// the same narrow edge. The negatives lock in that the edge is exactly
// quartermaster→pilot and nothing wider — a QM→non-pilot /clear (the
// blast-radius case the conservative default guards against) stays denied.
func TestResolve_PeerScope_EdgeRule_Quartermaster(t *testing.T) {
	// Positive: quartermaster→pilot returns the literal text.
	got, err := Resolve("clear", ScopePeer, "quartermaster", "pilot")
	if err != nil {
		t.Errorf("Resolve(clear, peer, quartermaster→pilot): want /clear, got error %v", err)
	}
	if got != "/clear" {
		t.Errorf("Resolve(clear, peer, quartermaster→pilot) = %q, want /clear", got)
	}

	// Negative: QM→non-pilot stays denied — the edge is narrow. Covers a
	// peer chamber (engineer) and a dispatcher peer (bosun) as recipients.
	for _, recipient := range []string{"engineer", "bosun", "shipwright", "surveyor"} {
		if _, err := Resolve("clear", ScopePeer, "quartermaster", recipient); !errors.Is(err, ErrScopeDenied) {
			t.Errorf("Resolve(clear, peer, quartermaster→%s): want ErrScopeDenied, got %v", recipient, err)
		}
	}
	// Negative: edge direction matters — reversed (pilot→quartermaster) denied.
	if _, err := Resolve("clear", ScopePeer, "pilot", "quartermaster"); !errors.Is(err, ErrScopeDenied) {
		t.Errorf("Resolve(clear, peer, pilot→quartermaster): want ErrScopeDenied, got %v", err)
	}
	// Negative: self scope — same as the Bosun case, PeerEdges isn't
	// consulted in self scope so clear's Self=false governs.
	if _, err := Resolve("clear", ScopeSelf, "quartermaster", "quartermaster"); !errors.Is(err, ErrScopeDenied) {
		t.Errorf("Resolve(clear, self, quartermaster→quartermaster): want ErrScopeDenied, got %v", err)
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
		"mcp-disable-tmux-tell", "mcp-enable-tmux-tell", "mcp-restart-tmux-tell",
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
		"mcp-disable-tmux-tell", "mcp-enable-tmux-tell", "mcp-restart-tmux-tell",
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
	wantPeer := []string{"help", "mcp-enable-tmux-tell", "mcp-restart-tmux-tell", "rename"}
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
	wantBosunPilot := []string{"clear", "help", "mcp-enable-tmux-tell", "mcp-restart-tmux-tell", "rename"}
	if len(bosunPilot) != len(wantBosunPilot) {
		t.Fatalf("bosun→pilot names = %v, want %v", bosunPilot, wantBosunPilot)
	}
	for i, n := range bosunPilot {
		if n != wantBosunPilot[i] {
			t.Fatalf("bosun→pilot[%d] = %q, want %q", i, n, wantBosunPilot[i])
		}
	}

	// Peer scope WITH the #167 edge match: quartermaster→pilot also
	// surfaces "clear", same as the Bosun edge.
	qmPilot := NamesForScope(ScopePeer, "quartermaster", "pilot")
	wantQMPilot := []string{"clear", "help", "mcp-enable-tmux-tell", "mcp-restart-tmux-tell", "rename"}
	if len(qmPilot) != len(wantQMPilot) {
		t.Fatalf("quartermaster→pilot names = %v, want %v", qmPilot, wantQMPilot)
	}
	for i, n := range qmPilot {
		if n != wantQMPilot[i] {
			t.Fatalf("quartermaster→pilot[%d] = %q, want %q", i, n, wantQMPilot[i])
		}
	}

	// And QM→non-pilot does NOT surface "clear" — the narrow edge holds
	// for the names listing too (mirrors the Resolve negatives).
	qmEngineer := NamesForScope(ScopePeer, "quartermaster", "engineer")
	for _, n := range qmEngineer {
		if n == "clear" {
			t.Fatalf("quartermaster→engineer should NOT surface clear; got %v", qmEngineer)
		}
	}
}

// TestAllowed_DescNonEmpty guards that every whitelisted command has a
// non-empty Desc, so a future verb can't ship a blank --help row (#583).
func TestAllowed_DescNonEmpty(t *testing.T) {
	for name, cmd := range Allowed {
		if cmd.Desc == "" {
			t.Errorf("Allowed[%q].Desc is empty — add a receiver-side description", name)
		}
	}
}
