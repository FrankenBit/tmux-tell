package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// newRefreshTestStore opens an in-memory store and seeds it with the
// given agent names. Returns the store; the caller is responsible for
// invoking Close via t.Cleanup if needed.
func newRefreshTestStore(t *testing.T, agents ...string) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	for i, name := range agents {
		if err := s.UpsertAgent(ctx, name, paneIDFor(i)); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return s
}

func paneIDFor(i int) string {
	// Deterministic pane ids %1, %2, … so the test output is
	// predictable across runs.
	if i < 0 {
		i = 0
	}
	return "%" + itoa10(i+1)
}

// itoa10 is a local int-to-decimal-string helper so the file doesn't
// need strconv for a single test-only formatting concern.
func itoa10(n int) string {
	if n == 0 {
		return "0"
	}
	var (
		buf [20]byte
		i   = len(buf)
		neg bool
	)
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestRefreshAllMcps_HappyPath fans out to three chambers, asserts
// each gets the disable+enable pair, and verifies the JSON shape.
func TestRefreshAllMcps_HappyPath(t *testing.T) {
	s := newRefreshTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	exit := runRefreshAllMcpsWithStore(ctx, s, "alice", "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q",
			exit, exitOK, stderr.String(), stdout.String())
	}

	var got refreshResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if !got.OK {
		t.Errorf("ok = false; want true")
	}
	if got.Sender != "alice" {
		t.Errorf("sender = %q, want %q", got.Sender, "alice")
	}
	if got.Total != 3 {
		t.Errorf("total = %d, want 3", got.Total)
	}
	if got.Queued != 3 {
		t.Errorf("queued = %d, want 3", got.Queued)
	}
	if got.Failed != 0 {
		t.Errorf("failed = %d, want 0", got.Failed)
	}
	// Chambers should be name-sorted (alice, bob, carol). Each entry
	// must carry a DisableID + EnableID and no Error.
	wantOrder := []string{"alice", "bob", "carol"}
	if len(got.Chambers) != len(wantOrder) {
		t.Fatalf("chambers = %d, want %d", len(got.Chambers), len(wantOrder))
	}
	for i, c := range got.Chambers {
		if c.Name != wantOrder[i] {
			t.Errorf("chambers[%d].name = %q, want %q", i, c.Name, wantOrder[i])
		}
		if !c.OK {
			t.Errorf("chambers[%d] (%s) ok = false: %s", i, c.Name, c.Error)
		}
		if c.DisableID == "" || c.EnableID == "" {
			t.Errorf("chambers[%d] (%s) missing macro public_ids: disable=%q enable=%q",
				i, c.Name, c.DisableID, c.EnableID)
		}
		if c.Error != "" {
			t.Errorf("chambers[%d] (%s) has Error=%q on success path", i, c.Name, c.Error)
		}
	}

	// Each chamber should have exactly 2 queued rows (the macro pair).
	for _, name := range wantOrder {
		depth, err := s.RecipientQueueDepth(ctx, name)
		if err != nil {
			t.Fatalf("depth %s: %v", name, err)
		}
		if depth != 2 {
			t.Errorf("%s queue depth = %d, want 2 (disable + enable)", name, depth)
		}
	}
}

// TestRefreshAllMcps_EmptyRegistry exercises the no-chambers-registered
// edge: zero rows in agents, no errors, summary line emitted.
func TestRefreshAllMcps_EmptyRegistry(t *testing.T) {
	s := newRefreshTestStore(t)
	ctx := context.Background()

	// No registered agents means we can't resolve a sender from the
	// registry either; pass the sender explicitly via the test path.
	// (The CLI's identity.Resolve would have failed earlier — this
	// test exercises the with-store core, which assumes sender is
	// already resolved.)
	var stdout, stderr bytes.Buffer
	exit := runRefreshAllMcpsWithStore(ctx, s, "alice", "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", exit, exitOK, stderr.String())
	}

	var got refreshResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if !got.OK || got.Total != 0 || got.Queued != 0 || got.Failed != 0 {
		t.Errorf("empty-registry shape = %+v, want ok=true total=0 queued=0 failed=0", got)
	}
	if len(got.Chambers) != 0 {
		t.Errorf("chambers = %d, want 0 on empty registry", len(got.Chambers))
	}
}

// TestRefreshAllMcps_PartialFailure pre-fills one chamber's queue to
// the recipient cap, then runs the fan-out. The over-capped chamber
// must fail; the others succeed; the summary must report failed=1
// and exit non-zero so a calling script can detect partial outcome.
func TestRefreshAllMcps_PartialFailure(t *testing.T) {
	s := newRefreshTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()

	// Pre-fill bob's queue to capRecipientQueue using alice → bob
	// regular messages. The macro will then fail for bob because
	// adding 2 control rows would overshoot the recipient cap. Alice
	// + carol still succeed.
	for i := 0; i < capRecipientQueue; i++ {
		if _, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: "carol", ToAgent: "bob",
			Body:              "pre-fill",
			MaxRecipientQueue: capRecipientQueue,
		}); err != nil {
			t.Fatalf("pre-fill %d: %v", i, err)
		}
	}

	var stdout, stderr bytes.Buffer
	exit := runRefreshAllMcpsWithStore(ctx, s, "alice", "json", &stdout, &stderr)
	if exit != exitInternal {
		t.Errorf("exit = %d, want %d (partial-failure should signal non-zero)",
			exit, exitInternal)
	}

	var got refreshResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if got.OK {
		t.Errorf("ok = true on partial failure; want false")
	}
	if got.Failed != 1 {
		t.Errorf("failed = %d, want 1", got.Failed)
	}
	if got.Queued != 2 {
		t.Errorf("queued = %d, want 2 (alice + carol succeed; bob fails)", got.Queued)
	}

	// Find bob's entry and verify it carries the Error string and
	// missing public_ids.
	var bobEntry *refreshChamberEntry
	for i := range got.Chambers {
		if got.Chambers[i].Name == "bob" {
			bobEntry = &got.Chambers[i]
			break
		}
	}
	if bobEntry == nil {
		t.Fatalf("bob not in chambers list: %+v", got.Chambers)
	}
	if bobEntry.OK {
		t.Errorf("bob.ok = true on cap-rejected path")
	}
	if bobEntry.Error == "" {
		t.Errorf("bob.error empty on cap-rejected path; expected ErrRecipientQueueFull text")
	}
	if bobEntry.DisableID != "" || bobEntry.EnableID != "" {
		t.Errorf("bob carries macro ids on failure: disable=%q enable=%q",
			bobEntry.DisableID, bobEntry.EnableID)
	}
}

// TestRefreshAllMcps_TextFormat verifies the operator-facing text
// rendering: summary line first, then one row per chamber with the
// macro ids on success or FAILED + error on failure.
func TestRefreshAllMcps_TextFormat(t *testing.T) {
	s := newRefreshTestStore(t, "alice", "bob")
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	exit := runRefreshAllMcpsWithStore(ctx, s, "alice", "text", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%q", exit, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "from=alice") {
		t.Errorf("text output missing sender: %s", out)
	}
	if !strings.Contains(out, "total=2") {
		t.Errorf("text output missing total: %s", out)
	}
	if !strings.Contains(out, "queued=2") {
		t.Errorf("text output missing queued count: %s", out)
	}
	if !strings.Contains(out, "alice") || !strings.Contains(out, "bob") {
		t.Errorf("text output missing per-chamber rows: %s", out)
	}
}

// TestRefreshAllMcps_RejectsUnknownFormat ensures the --format flag's
// validator covers the unknown-value path.
func TestRefreshAllMcps_RejectsUnknownFormat(t *testing.T) {
	s := newRefreshTestStore(t, "alice")
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	exit := runRefreshAllMcpsWithStore(ctx, s, "alice", "yaml", &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want %d for unknown --format", exit, exitUsage)
	}
	if !strings.Contains(stdout.String(), "unknown --format") {
		t.Errorf("error message missing 'unknown --format': %s", stdout.String())
	}
}
