package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/budget"
)

func budgetParams(agent string, recipients ...string) BudgetParams {
	return BudgetParams{
		Agent:         agent,
		Recipients:    recipients,
		Bytes:         0,
		Capacity:      budget.DefaultCap,
		RefillPerHour: budget.DefaultRefillPerHour,
		Alpha:         budget.DefaultAlpha,
		Window:        budget.DefaultWindow,
	}
}

// t0 is a fixed instant. Every test drives time explicitly rather than
// calling time.Now(), so a slow CI box cannot refill a balance mid-test.
var t0 = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// TestBudgetSurvivesReRegistration is the #918 AC, and it is deliberately
// written as a BEHAVIOURAL test even though the property is structural.
//
// The structural argument — `budgets` is its own table, `register` writes
// `agents` only, so there is no code path that could drop the balance — is
// the real guarantee and it is stated at the schema. This test is the
// control for it: if someone later adds an FK with ON DELETE CASCADE, or
// moves the balance onto `agents`, the argument silently stops holding and
// nothing else would notice. A by-construction claim still needs a needle
// that would go red if the construction changed.
func TestBudgetSurvivesReRegistration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("register: %v", err)
	}
	before, err := s.ChargeBudget(ctx, t0, budgetParams("alice", "bob", "carol"))
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if before.Cost <= 0 {
		t.Fatalf("charge cost must be positive, got %v", before.Cost)
	}

	// Full unregister → re-register cycle, the thing the AC names.
	if _, err := s.DeleteAgent(ctx, "alice"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if err := s.UpsertAgent(ctx, "alice", "%9"); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	after, err := s.QuoteBudget(ctx, t0, budgetParams("alice"))
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if after.Balance != before.Remaining {
		t.Fatalf("balance did not survive re-registration: charged to %v, read back %v",
			before.Remaining, after.Balance)
	}
	if after.Balance == after.Capacity {
		t.Fatalf("balance reset to full capacity %v — the budget was dropped, not preserved", after.Capacity)
	}
}

// TestChargeAndCheckAreAtomic pins the reason charge and check share a
// transaction. Ten concurrent fan-outs race for a balance that affords
// only a few; the invariant is that the number that SUCCEED is exactly
// the number the balance could pay for. A check-then-act implementation
// lets extra sends through — the balance goes further negative than the
// overdraft floor permits.
func TestChargeAndCheckAreAtomic(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	p := budgetParams("alice", "bob", "carol", "dave")
	// Capacity 20 against a ~9-cost fan-out: two charges fit above the -5
	// overdraft floor and the rest must be refused. Sized deliberately so the
	// run is PARTIAL — an all-succeed run cannot distinguish an atomic
	// implementation from a racy one, which is what the guard below asserts.
	p.Capacity = 20
	p.RefillPerHour = 0 // freeze refill: the only variable is the race

	const racers = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ChargeBudget(ctx, t0, p); err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	final, err := s.QuoteBudget(ctx, t0, budgetParams("alice"))
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	floor := budget.Overdraft(p.Capacity)
	if final.Balance < floor {
		t.Fatalf("balance %v breached the overdraft floor %v after %d concurrent charges (%d succeeded) — check and charge are not atomic",
			final.Balance, floor, racers, okCount)
	}
	if okCount == 0 {
		t.Fatalf("no charge succeeded; the test proves nothing")
	}
	if okCount == racers {
		t.Fatalf("all %d charges succeeded against a capacity of %v — nothing was refused, so the invariant is untested", racers, p.Capacity)
	}
}

// TestWindowIsDerivedFromMessages pins that breadth is charged on DISTINCT
// recipients within the window, read out of `messages` rather than out of a
// second table. Reaching a NEW recipient costs more than repeating to one
// already paid for.
func TestWindowIsDerivedFromMessages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seeded with an EXPLICIT created_at rather than through InsertMessage:
	// that writes SQLite's strftime('now'), and this test drives a synthetic
	// clock. Comparing a hand-set `now` against real-clock rows is what made
	// the first draft of this test report a window that never expired — the
	// rows were three hours NEWER than the cutoff, not older.
	for i, to := range []string{"bob", "carol", "dave"} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO messages (public_id, from_agent, to_agent, body, state, created_at)
			 VALUES (?, 'alice', ?, 'x', ?, ?)`,
			"seed"+string(rune('a'+i)), to, StateDelivered,
			t0.Add(-time.Minute).UTC().Format(tsLayout)); err != nil {
			t.Fatalf("seed %s: %v", to, err)
		}
	}

	repeat, err := s.QuoteBudget(ctx, t0, budgetParams("alice", "bob"))
	if err != nil {
		t.Fatalf("quote repeat: %v", err)
	}
	fresh, err := s.QuoteBudget(ctx, t0, budgetParams("alice", "erin"))
	if err != nil {
		t.Fatalf("quote fresh: %v", err)
	}

	if repeat.PriorRecipients != 3 || repeat.NewRecipients != 3 {
		t.Fatalf("repeat should add no breadth: prior=%d new=%d", repeat.PriorRecipients, repeat.NewRecipients)
	}
	if fresh.NewRecipients != 4 {
		t.Fatalf("a new recipient should widen the window: new=%d", fresh.NewRecipients)
	}
	if !(fresh.Cost > repeat.Cost) {
		t.Fatalf("reaching a new recipient must cost more than repeating: fresh=%v repeat=%v", fresh.Cost, repeat.Cost)
	}

	// Outside the window the same send is cheap again — the window is a
	// window, not a ledger.
	later, err := s.QuoteBudget(ctx, t0.Add(2*budget.DefaultWindow), budgetParams("alice", "erin"))
	if err != nil {
		t.Fatalf("quote later: %v", err)
	}
	if later.PriorRecipients != 0 {
		t.Fatalf("window did not expire: prior=%d", later.PriorRecipients)
	}
}

// TestRefusedRowsDoNotRaiseThePriceOfTheRetry is the one that would be easy
// to get wrong in the direction nobody notices. A message refused by a cap
// reached NOBODY. Counting it as breadth would mean a refusal makes the
// retry more expensive than the original attempt — a budget that punishes
// you for being refused.
func TestRefusedRowsDoNotRaiseThePriceOfTheRetry(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, state, created_at)
		 VALUES ('zzz1', 'alice', 'mallory', 'x', ?, ?)`,
		StateRefused, t0.Add(-time.Minute).UTC().Format(tsLayout)); err != nil {
		t.Fatalf("seed refused row: %v", err)
	}

	q, err := s.QuoteBudget(ctx, t0, budgetParams("alice", "mallory"))
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if q.PriorRecipients != 0 {
		t.Fatalf("a refused row was charged as breadth: prior=%d", q.PriorRecipients)
	}

	// Control: the SAME row in a delivered state DOES count, so the test
	// above is discriminating rather than merely finding an empty window.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE messages SET state = ? WHERE public_id = 'zzz1'`, StateDelivered); err != nil {
		t.Fatalf("flip state: %v", err)
	}
	q2, err := s.QuoteBudget(ctx, t0, budgetParams("alice", "mallory"))
	if err != nil {
		t.Fatalf("quote 2: %v", err)
	}
	if q2.PriorRecipients != 1 {
		t.Fatalf("control failed: a delivered row must count as breadth, prior=%d", q2.PriorRecipients)
	}
}

// TestRefusalNamesBalanceCostAndTimeToAfford is the #918 AC on the refusal
// message. A refusal that only says "no" leaves the sender unable to choose
// between waiting, trimming and splitting — all three of which are things
// the cost function actually rewards.
func TestRefusalNamesBalanceCostAndTimeToAfford(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p := budgetParams("alice", "bob", "carol", "dave", "erin", "frank")
	p.Capacity = 5 // deliberately tiny: the first fan-out cannot be afforded
	p.RefillPerHour = 10

	st, err := s.ChargeBudget(ctx, t0, p)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("expected ErrBudgetExhausted, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"alice", "has", "costs", "affordable in"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal does not name %q: %s", want, msg)
		}
	}
	if st.TimeToAfford <= 0 {
		t.Fatalf("refusal must carry a positive time-to-afford, got %v", st.TimeToAfford)
	}
	if st.Cost <= 0 || st.Balance <= 0 {
		t.Fatalf("refusal state must carry both figures: balance=%v cost=%v", st.Balance, st.Cost)
	}

	// A refusal must NOT debit. Otherwise being refused makes the next
	// attempt harder, which is the same defect as charging for refused rows.
	after, err := s.QuoteBudget(ctx, t0, budgetParams("alice"))
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	after.Capacity = p.Capacity
	if q, _ := s.QuoteBudget(ctx, t0, p); q.Balance != st.Balance {
		t.Fatalf("a refusal debited the balance: before=%v after=%v", st.Balance, q.Balance)
	}
}

// TestLazyRefillRestoresOverTime pins that the balance recovers with no
// timer and no sweeper: it is only ever observed through the read path, so
// refilling at read time is indistinguishable from refilling continuously —
// and it cannot drift while the process is down.
func TestLazyRefillRestoresOverTime(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p := budgetParams("alice", "bob", "carol", "dave")
	spent, err := s.ChargeBudget(ctx, t0, p)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	same, err := s.QuoteBudget(ctx, t0, budgetParams("alice"))
	if err != nil {
		t.Fatalf("quote same instant: %v", err)
	}
	if same.Balance != spent.Remaining {
		t.Fatalf("no time passed, balance moved: %v → %v", spent.Remaining, same.Balance)
	}

	later, err := s.QuoteBudget(ctx, t0.Add(time.Hour), budgetParams("alice"))
	if err != nil {
		t.Fatalf("quote later: %v", err)
	}
	if !(later.Balance > same.Balance) {
		t.Fatalf("balance did not refill over an hour: %v → %v", same.Balance, later.Balance)
	}
	if later.Balance > later.Capacity {
		t.Fatalf("refill overshot the cap: %v > %v", later.Balance, later.Capacity)
	}
}

// TestRefundBudget covers the ONE case a refund exists for, and the three
// it must not invent.
//
// The motivating instance was found by a smoke test, not by design: a 4-way
// fan-out was charged 2.0 and then refused for every recipient by the
// sender-backlog cap. The balance had moved and nothing had been delivered.
func TestRefundBudget(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p := budgetParams("alice", "bob", "carol", "dave")
	spent, err := s.ChargeBudget(ctx, t0, p)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	// The case it exists for: credit the exact charge back.
	if err := s.RefundBudget(ctx, t0, "alice", spent.Cost, p.Capacity); err != nil {
		t.Fatalf("refund: %v", err)
	}
	back, err := s.QuoteBudget(ctx, t0, budgetParams("alice"))
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if back.Balance != p.Capacity {
		t.Fatalf("refund did not restore the balance: %v (want %v)", back.Balance, p.Capacity)
	}

	// It must not push ABOVE capacity — a refund is a credit, not a bonus.
	if err := s.RefundBudget(ctx, t0, "alice", 1000, p.Capacity); err != nil {
		t.Fatalf("refund overshoot: %v", err)
	}
	over, err := s.QuoteBudget(ctx, t0, budgetParams("alice"))
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if over.Balance != p.Capacity {
		t.Fatalf("refund overshot the cap: %v > %v", over.Balance, p.Capacity)
	}

	// A refund for an agent that was never charged must NOT create a row.
	// Creating one would invent a balance for an agent whose budget has
	// never been observed, and the invented value would then be treated as
	// authoritative by every later read.
	if err := s.RefundBudget(ctx, t0, "nobody", 5, p.Capacity); err != nil {
		t.Fatalf("refund unknown agent: %v", err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM budgets WHERE agent = 'nobody'`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("refund invented a budget row for an agent that never spent: %d rows", rows)
	}

	// Control, and it is the discriminating one: a charge must still STICK.
	// Every assertion above would also pass against an implementation that
	// simply never charges — this is the arm that excludes it.
	fresh, err := s.ChargeBudget(ctx, t0, p)
	if err != nil {
		t.Fatalf("charge again: %v", err)
	}
	stuck, err := s.QuoteBudget(ctx, t0, budgetParams("alice"))
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if stuck.Balance != fresh.Remaining || stuck.Balance >= p.Capacity {
		t.Fatalf("charge did not stick: balance=%v remaining=%v capacity=%v",
			stuck.Balance, fresh.Remaining, p.Capacity)
	}
}
