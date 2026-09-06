package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// tmux-tell#933, the last open AC: RECORD a mutation arm for the REGISTER FIELD.
//
// 🔴 THE AC'S PREMISE WAS THAT ONLY THE RECORDING WAS MISSING — that the surface
// was already sensitive because three arms had been run against it. Measured, the
// sensitivity was in the STORE QUERY (RefusedInbound), one layer down. The register
// FIELD had no test at all: three mutations to internal/cli/register.go all survived
// a full green `go test ./internal/store/ ./internal/cli/`.
//
//	gate -> false          the field is NEVER emitted        SURVIVED
//	Total >= 0             an honest zero is emitted         SURVIVED
//	recent/total swapped   the two numbers trade places      SURVIVED
//
// Each was confirmed applied by `git diff --stat`, and the cli package re-ran
// (27s, not "cached") in every arm — so the mutants were genuinely exercised
// rather than skipped by the build cache. That last check matters: if both
// packages had reported cached, three green runs would have proved nothing.
//
// This test closes that. Its arms are keyed to those three mutants.
func TestRegisterEmitsRefusedInbound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "messages.db")

	seed, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	if err := seed.UpsertAgent(ctx, "bob", "%1"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Fill bob's queue to its cap, then one more: the overflow is REFUSED and
	// written as a StateRefused durability row addressed TO bob.
	const cap = 2
	for i := 0; i < cap; i++ {
		if _, err := seed.InsertMessage(ctx, store.InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "filler", MaxRecipientQueue: cap,
		}); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if _, err := seed.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "refused", MaxRecipientQueue: cap,
	}); err == nil {
		t.Fatal("the over-cap send should have been REFUSED; it succeeded, so this test " +
			"would assert against a store with no refused row and pass for the wrong reason")
	}
	_ = seed.Close()

	// 🔑 MAKE recent AND total DIFFER, or the swap mutant is invisible.
	//
	// With a single refusal inside the window both counts are 1, so exchanging
	// them changes nothing observable and an arm comparing each to the store's
	// answer still passes. Backdating one refused row past the 24h window gives
	// total=2, recent=1 — two values that cannot be swapped silently.
	//
	// This is the decoy-arm shape from CLAUDE.md: adding the hazardous input is
	// not enough when the expected answer coincides with the broken one. I hit it
	// here: the first version of this test predicted the limitation in a comment
	// and shipped anyway, and the swap mutant survived a green run.
	backdate(t, dbPath, "bob")

	t.Setenv("CLAUDE_MSG_DB", dbPath)

	register := func(t *testing.T, name string) map[string]any {
		t.Helper()
		var stdout, stderr bytes.Buffer
		exit := runRegisterCLI([]string{
			"--name", name, "--pane", "%1", "--force", "--start-mailman=false",
		}, &stdout, &stderr)
		if exit != exitOK {
			t.Fatalf("register exit = %d, want exitOK; stderr=%s", exit, stderr.String())
		}
		var out map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("register stdout is not JSON: %v\n%s", err, stdout.String())
		}
		return out
	}

	// ARM 1 — the recipient sees it. Kills "gate -> false".
	out := register(t, "bob")
	raw, ok := out["refused_inbound"]
	if !ok {
		t.Fatalf("refused_inbound absent from bob's register; got keys %v", keysOf(out))
	}
	field, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("refused_inbound is %T, want an object", raw)
	}

	// ARM 2 — the two numbers are not interchangeable. Kills "recent/total swapped"
	// only if they DIFFER, and in this fixture they do not: one refusal inside the
	// window makes both 1. So assert them against the store's own answer instead of
	// against each other, which is what makes the arm discriminating rather than
	// tautological.
	check, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	want, err := check.RefusedInbound(ctx, "bob")
	_ = check.Close()
	if err != nil {
		t.Fatalf("RefusedInbound: %v", err)
	}
	if want.Total == want.Recent {
		t.Fatalf("fixture defect: total(%d) == recent(%d), so a swap of the two is "+
			"undetectable and the arms below cannot discriminate", want.Total, want.Recent)
	}
	if got := numeric(t, field["total"]); got != int64(want.Total) {
		t.Errorf("refused_inbound.total = %d, want %d (the store's own count)", got, want.Total)
	}
	if got := numeric(t, field["recent"]); got != int64(want.Recent) {
		t.Errorf("refused_inbound.recent = %d, want %d (the store's own count)", got, want.Recent)
	}
	if field["newest_at"] == nil || field["newest_at"] == "" {
		t.Error("refused_inbound.newest_at is empty — the reader cannot tell an old refusal from a live one")
	}
	if field["window"] == nil || field["window"] == "" {
		t.Error("refused_inbound.window is empty — `recent` is uninterpretable without it")
	}

	// ARM 3 — ABSENT on an agent with nothing refused. Kills "Total >= 0".
	//
	// ⚠️ This is the arm a naive test omits, and it pins a DELIBERATE decision:
	// register.go emits the field only when Total > 0, because "a field that is
	// present-and-zero on every register is the shape a reader stops seeing".
	// A test that only checks the populated case would let that decision be
	// reversed silently — the field would still be correct, and unread.
	seed2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen for carol: %v", err)
	}
	if err := seed2.UpsertAgent(ctx, "carol", "%2"); err != nil {
		t.Fatalf("seed carol: %v", err)
	}
	_ = seed2.Close()

	outCarol := register(t, "carol")
	if _, present := outCarol["refused_inbound"]; present {
		t.Errorf("refused_inbound present for an agent with NO refusals: %v\n"+
			"An honest zero must be absent, not zero — see register.go's own note.",
			outCarol["refused_inbound"])
	}
	if _, present := outCarol["refused_inbound_error"]; present {
		t.Errorf("refused_inbound_error present on a healthy store: %v", outCarol["refused_inbound_error"])
	}
}

// backdate inserts one more REFUSED row for the agent and ages it past the
// window, so RefusedInbound reports total=2 recent=1. Written with raw SQL
// because the store deliberately offers no way to forge a timestamp.
func backdate(t *testing.T, dbPath, agent string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = db.Close() }()

	old := time.Now().UTC().Add(-48 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	res, err := db.Exec(
		`INSERT INTO messages (public_id, from_agent, to_agent, body, state, created_at)
		 SELECT 'aged01', from_agent, to_agent, 'aged refusal', state, ?
		   FROM messages WHERE to_agent = ? AND state = 'refused' LIMIT 1`,
		old, agent)
	if err != nil {
		t.Fatalf("insert aged refusal: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		t.Fatalf("aged refusal insert affected %d rows, want 1 — the fixture has no "+
			"refused row to clone, so total and recent would still be equal", n)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// numeric accepts whatever JSON decoded the count as; a float64 that is not a
// whole number is a defect rather than a rounding question, so it is reported.
func numeric(t *testing.T, v any) int64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("count is %T (%v), want a JSON number", v, v)
	}
	if f != float64(int64(f)) {
		t.Fatalf("count %v is not a whole number", f)
	}
	return int64(f)
}
