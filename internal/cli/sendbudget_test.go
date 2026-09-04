package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/budget"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// #950: the communication budget charged the CLI send path only. Every chamber
// sends through the MCP tool, so it had never charged one — on `main@6ebbc6b`
// internal/cli/mcp.go carried ZERO budget references against 13 in send.go.
//
// 🔑 THE ARMS BELOW ARE DIFFERENTIAL *AND* ANCHORED, AND THE ANCHOR IS THE HALF
// THAT MATTERS. A pure differential — "both surfaces charge the same" — PASSES
// ON THE DEFECT: before the fix the CLI charged c and MCP charged 0, but with
// the budget off (the shipped default until v0.38.0) both charge 0, and 0 == 0
// is a clean green. Every arm here therefore asserts the balance MOVED before
// it asserts the two surfaces agree.

// newIsolatedTestStore returns a store that does NOT share state with any other.
//
// 🔴 newCmdTestStore CANNOT BE USED FOR A DIFFERENTIAL, AND THE REASON IS
// INVISIBLE AT THE CALLSITE. store.Open(":memory:") builds the DSN
// "file::memory:?cache=shared", so every in-memory store in the PROCESS is the
// SAME DATABASE — two `newCmdTestStore` calls return two handles onto one set
// of rows. Measured while writing this file: a send charged through handle A
// moved the balance read through handle B (120.0000 → 118.7450 → the next
// store's "before" read at 118.7450).
//
// ⚠️ THAT TURNS A DIFFERENTIAL INTO A MIRROR. With one shared database both
// "before" reads return the same number and both "after" reads return the same
// number, so `cliCharged == mcpCharged` HOLDS BY CONSTRUCTION — including on the
// broken tree, where MCP charged nothing and both deltas were simply the CLI's
// charge. The arm was green against the defect it was written to catch, and only
// the mutation run below found it. A check whose two sides come from one source
// passes in every world (/srv/CLAUDE.md §a review's commit_id).
func newIsolatedTestStore(t *testing.T, agents ...string) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, name := range agents {
		if err := s.UpsertAgent(context.Background(), name, "%99"); err != nil {
			t.Fatalf("seed agent %s: %v", name, err)
		}
	}
	return s
}

// budgetOn returns tunables with the shipped calibration and the gate enabled.
//
// ⚠️ REFILL IS ZERO, AND THAT IS THE INSTRUMENT, NOT THE POLICY. Refill is
// LAZY — a balance is brought forward to the moment it is read — so a
// before/after difference taken with refill ON measures (charge − refill over
// the wall-clock between the two reads), not the charge. At the shipped 16/hour
// that is a few thousandths over a test, which is invisible at %.4f and still
// large enough to make two identical charges compare unequal. The first run of
// the differential arm below failed reporting "CLI charged 1.5409 but MCP
// charged 1.5409" — two numbers that print the same and are not, which is the
// tell that the reading is time-dependent rather than the charge.
func budgetOn() budgetTunables {
	return budgetTunables{
		Enabled:       true,
		Capacity:      budget.DefaultCap,
		RefillPerHour: 0,
		Alpha:         budget.DefaultAlpha,
		Window:        budget.DefaultWindow,
	}
}

// balanceOf reads a sender's balance without charging it. It quotes with the
// SAME tunables the send used, because QuoteBudget brings the balance forward
// at the rate it is handed — reading with a different rate than was charged
// answers a different question.
func balanceOf(t *testing.T, s *store.Store, b budgetTunables, agent string) float64 {
	t.Helper()
	st, err := s.QuoteBudget(context.Background(), time.Now(), b.paramsFor(agent, []string{"nobody"}, ""))
	if err != nil {
		t.Fatalf("quote %s: %v", agent, err)
	}
	return st.Balance
}

// TestBothSendSurfacesChargeTheSameAmount is the #950 differential: one message,
// two surfaces, two stores, and the charge must be identical and non-zero.
func TestBothSendSurfacesChargeTheSameAmount(t *testing.T) {
	const body = "a body long enough that the length factor is not 1.0 — several dozen bytes of it"

	cliStore := newIsolatedTestStore(t, "alice", "bob")
	mcpStore := newIsolatedTestStore(t, "alice", "bob")
	ctx := context.Background()

	cliBefore := balanceOf(t, cliStore, budgetOn(), "alice")
	mcpBefore := balanceOf(t, mcpStore, budgetOn(), "alice")

	var stdout, stderr strings.Builder
	if code := runSendWithStore(ctx, cliStore, sendParams{
		From: "alice", To: "bob", Body: body, Budget: budgetOn(),
	}, &stdout, &stderr); code != exitOK {
		t.Fatalf("CLI send: exit %d, stderr=%q", code, stderr.String())
	}
	if _, err := doSendMCP(ctx, mcpStore, sendParams{
		From: "alice", To: "bob", Body: body, Budget: budgetOn(),
	}); err != nil {
		t.Fatalf("MCP send: %v", err)
	}

	cliCharged := cliBefore - balanceOf(t, cliStore, budgetOn(), "alice")
	mcpCharged := mcpBefore - balanceOf(t, mcpStore, budgetOn(), "alice")

	// THE ANCHOR. Without this the pre-fix tree passes: 0 == 0.
	if mcpCharged <= 0 {
		t.Fatalf("the MCP send charged %.4f — the budget did not move, which is #950 itself", mcpCharged)
	}
	if cliCharged <= 0 {
		t.Fatalf("the CLI send charged %.4f — the budget did not move", cliCharged)
	}
	// THE DIFFERENTIAL. Compared against each other, not against a constant, so
	// it keeps holding when the cost function or the calibration changes.
	if !nearlyEqual(cliCharged, mcpCharged) {
		t.Errorf("CLI charged %.4f but MCP charged %.4f — the two send surfaces price differently",
			cliCharged, mcpCharged)
	}
}

// TestMCPSendHandlerResolvesTheBudgetFromConfig covers the WIRING, which the
// differential above cannot reach: that arm hands doSendMCP a Budget directly,
// so it stays green if the handler stops resolving one.
//
// 🔑 THAT IS EXACTLY THE SHAPE OF #950. The handler read config in this very
// block — for max-recipients-per-send — and resolved no budget key at all, so
// the block LOOKED like it was reading config while one knob reached chambers
// and the others did not.
func TestMCPSendHandlerResolvesTheBudgetFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[defaults]\nbudget-enabled = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TELL_CONFIG", cfgPath)

	s := newIsolatedTestStore(t, "alice", "bob")
	ctx := withInjectedIdentity(context.Background(), "alice")
	before := balanceOf(t, s, budgetOn(), "alice")

	if _, err := mcpSendHandler(s)(ctx, []byte(`{"to":"bob","body":"through the real handler"}`)); err != nil {
		t.Fatalf("handler send: %v", err)
	}
	if after := balanceOf(t, s, budgetOn(), "alice"); after >= before {
		t.Errorf("balance %.4f → %.4f: the handler did not resolve budget-enabled from config",
			before, after)
	}
}

// TestMCPSendHandlerLeavesTheBudgetAloneWhenDisabled is the negative control for
// the arm above: a guard that charged unconditionally would pass that one.
func TestMCPSendHandlerLeavesTheBudgetAloneWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[defaults]\nbudget-enabled = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TELL_CONFIG", cfgPath)

	s := newIsolatedTestStore(t, "alice", "bob")
	ctx := withInjectedIdentity(context.Background(), "alice")
	before := balanceOf(t, s, budgetOn(), "alice")

	if _, err := mcpSendHandler(s)(ctx, []byte(`{"to":"bob","body":"budget off"}`)); err != nil {
		t.Fatalf("handler send: %v", err)
	}
	if after := balanceOf(t, s, budgetOn(), "alice"); after != before {
		t.Errorf("balance moved %.4f → %.4f with budget-enabled=0", before, after)
	}
}

// TestMCPFanOutChargesOnceAndMatchesTheCLI pins the fan-out half. Breadth is
// super-linear, so a 2-recipient send must cost MORE than a 1-recipient one —
// which also proves the charge saw the whole fan-out rather than one leg.
func TestMCPFanOutChargesOnceAndMatchesTheCLI(t *testing.T) {
	const body = "fan-out body"
	ctx := context.Background()

	cliStore := newIsolatedTestStore(t, "alice", "bob", "carol")
	mcpStore := newIsolatedTestStore(t, "alice", "bob", "carol")
	singleStore := newIsolatedTestStore(t, "alice", "bob")

	cliBefore := balanceOf(t, cliStore, budgetOn(), "alice")
	mcpBefore := balanceOf(t, mcpStore, budgetOn(), "alice")
	singleBefore := balanceOf(t, singleStore, budgetOn(), "alice")

	var stdout, stderr strings.Builder
	if code := runMultiSendWithStore(ctx, cliStore, sendParams{
		From: "alice", ToRecipients: []string{"bob", "carol"}, Body: body, Budget: budgetOn(),
	}, &stdout, &stderr); code != exitOK {
		t.Fatalf("CLI fan-out: exit %d stderr=%q", code, stderr.String())
	}
	if _, err := doMultiSendMCP(ctx, mcpStore, sendParams{
		From: "alice", ToRecipients: []string{"bob", "carol"}, Body: body, Budget: budgetOn(),
	}); err != nil {
		t.Fatalf("MCP fan-out: %v", err)
	}
	if _, err := doSendMCP(ctx, singleStore, sendParams{
		From: "alice", To: "bob", Body: body, Budget: budgetOn(),
	}); err != nil {
		t.Fatalf("MCP single: %v", err)
	}

	cliCharged := cliBefore - balanceOf(t, cliStore, budgetOn(), "alice")
	mcpCharged := mcpBefore - balanceOf(t, mcpStore, budgetOn(), "alice")
	singleCharged := singleBefore - balanceOf(t, singleStore, budgetOn(), "alice")

	if mcpCharged <= 0 {
		t.Fatalf("the MCP fan-out charged %.4f — nothing was priced", mcpCharged)
	}
	if !nearlyEqual(cliCharged, mcpCharged) {
		t.Errorf("fan-out: CLI charged %.4f, MCP charged %.4f", cliCharged, mcpCharged)
	}
	if mcpCharged <= singleCharged {
		t.Errorf("2 recipients charged %.4f, 1 recipient charged %.4f — breadth did not engage, "+
			"so the charge saw one leg rather than the whole fan-out", mcpCharged, singleCharged)
	}
}

// TestMCPSendRefusesWhenExhausted pins that the refusal REACHES the MCP caller,
// carrying the same advice the CLI prints. A refusal a chamber cannot read is
// indistinguishable from an unexplained failure.
func TestMCPSendRefusesWhenExhausted(t *testing.T) {
	s := newIsolatedTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()
	b := budgetOn()
	b.Capacity = 4 // floor is -0.25 × capacity = -1: one send fits, the second cannot

	// 🔑 THE SECOND SEND GOES TO A DIFFERENT RECIPIENT ON PURPOSE. Breadth is
	// charged once per distinct recipient per window, so a REPEAT to bob costs
	// only the alpha term (~0.58 here) and would still be affordable — the arm
	// would then pass for the wrong reason on any capacity that refuses at all.
	body := strings.Repeat("x", 4000)
	if _, err := doSendMCP(ctx, s, sendParams{From: "alice", To: "bob", Body: body, Budget: b}); err != nil {
		t.Fatalf("first send should fit: %v", err)
	}
	_, err := doSendMCP(ctx, s, sendParams{From: "alice", To: "carol", Body: body, Budget: b})
	if err == nil {
		t.Fatal("second send was not refused — the exhausted budget did not reach the MCP caller")
	}
	if !strings.Contains(err.Error(), "splitting this into separate sends does NOT help") {
		t.Errorf("refusal = %q, want it to carry the shared advice text", err.Error())
	}
}

// TestConfigLoadFailureIsReportedNotSwallowed covers AC3. A malformed config
// silently falls back to compiled defaults, WIDENING every limit the operator
// set; the send still goes through (refusing would take the whole bus down over
// one file) but it must say so.
func TestConfigLoadFailureIsReportedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("this is not valid toml = = =\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TELL_CONFIG", cfgPath)

	s := newIsolatedTestStore(t, "alice", "bob")
	ctx := withInjectedIdentity(context.Background(), "alice")
	out, err := mcpSendHandler(s)(ctx, []byte(`{"to":"bob","body":"config is broken"}`))
	if err != nil {
		t.Fatalf("a broken config must not fail the send: %v", err)
	}
	resp, okCast := out.(SendResponse)
	if !okCast {
		t.Fatalf("unexpected response type %T", out)
	}
	if len(resp.Warnings) == 0 {
		t.Fatal("no warning: a config that failed to load left every configured limit unenforced, silently")
	}
	if !strings.Contains(strings.Join(resp.Warnings, " "), "NOT in force") {
		t.Errorf("warnings = %v, want them to name what is no longer enforced", resp.Warnings)
	}
}

func nearlyEqual(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
