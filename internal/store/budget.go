package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/budget"
)

// ErrBudgetExhausted is returned by ChargeBudget when the sender cannot
// afford the proposed send. The wrapped message names the balance, the
// cost and the time-to-afford (#918 AC) — a refusal that only says "no"
// leaves the sender with no way to choose between waiting, trimming and
// splitting.
var ErrBudgetExhausted = errors.New("store: communication budget exhausted")

// BudgetParams describes a proposed send, plus the tunables the caller
// resolved (config file → per-agent override → hardcoded default, via
// internal/config.ResolveInt). The store does not read config itself.
type BudgetParams struct {
	Agent      string
	Recipients []string
	Bytes      int

	Capacity      float64
	RefillPerHour float64
	Alpha         float64
	Window        time.Duration
}

// BudgetState is the full answer to "what would this send cost, and can
// I afford it?". Every field the refusal message needs is here, so the
// quote path and the charge path cannot drift in what they report.
type BudgetState struct {
	Agent    string
	Balance  float64 // after lazy refill, BEFORE the proposed send
	Capacity float64

	// The cost decomposition, so `--dry-run` can show WHY a send is
	// expensive rather than only that it is.
	PriorRecipients int // distinct recipients already paid for in the window
	NewRecipients   int // distinct recipients after this send
	Deliveries      int
	Bytes           int
	Cost            float64

	Affordable   bool
	TimeToAfford time.Duration

	// Remaining is the balance the send would leave behind. Reported even
	// when the send is refused (it is then negative or below the floor),
	// because "how far short am I" is what decides between trimming and
	// waiting.
	Remaining float64
}

// QuoteBudget computes what a send WOULD cost without charging for it.
// Read-only: `budget` and `send --dry-run` both land here.
func (s *Store) QuoteBudget(ctx context.Context, now time.Time, p BudgetParams) (BudgetState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BudgetState{}, fmt.Errorf("store: quote budget: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	st, err := quoteBudgetInTx(ctx, tx, now, p)
	if err != nil {
		return BudgetState{}, err
	}
	return st, nil
}

// ChargeBudget checks affordability and debits the balance in a SINGLE
// transaction.
//
// 🔑 WHY CHECK AND CHARGE MUST SHARE A TRANSACTION, and why this is not
// paranoia: two concurrent fan-outs that each read the balance, each find
// it sufficient, and each then debit will BOTH go through — the classic
// check-then-act race, and on a bus where fan-out is the expensive verb
// it is the exact case the budget exists to bound. The store opens with
// _txlock=immediate, so BEGIN takes the RESERVED lock and the read below
// is consistent with the UPDATE that follows it (same guarantee
// checkCapsInTx documents for the cap counts).
//
// Returns ErrBudgetExhausted with a fully-populated BudgetState when the
// send is unaffordable; the state is returned in BOTH the success and the
// refusal case, so a caller rendering the refusal does not have to make a
// second call against a balance that has since refilled.
func (s *Store) ChargeBudget(ctx context.Context, now time.Time, p BudgetParams) (BudgetState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BudgetState{}, fmt.Errorf("store: charge budget: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	st, err := quoteBudgetInTx(ctx, tx, now, p)
	if err != nil {
		return BudgetState{}, err
	}
	if !st.Affordable {
		return st, fmt.Errorf("%w: %s has %.1f, this send costs %.1f (%d deliveries, %d new recipient(s), %d bytes); affordable %s",
			ErrBudgetExhausted, p.Agent, st.Balance, st.Cost,
			st.Deliveries, st.NewRecipients-st.PriorRecipients, st.Bytes,
			whenAffordable(st.TimeToAfford, p.RefillPerHour))
	}

	ts := now.UTC().Format(tsLayout)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO budgets (agent, balance, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(agent) DO UPDATE SET balance = excluded.balance, updated_at = excluded.updated_at`,
		p.Agent, st.Remaining, ts); err != nil {
		return BudgetState{}, fmt.Errorf("store: charge budget: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return BudgetState{}, fmt.Errorf("store: charge budget commit: %w", err)
	}
	st.Balance = st.Remaining
	return st, nil
}

// tsLayout matches the schema's strftime('%Y-%m-%dT%H:%M:%fZ') so Go-written
// and SQLite-written timestamps sort against each other lexically.
const tsLayout = "2006-01-02T15:04:05.000Z"

// quoteBudgetInTx is the single place the cost and the balance are computed.
// Both the read-only quote and the charging path call it, so a `--dry-run`
// figure and the figure a refusal quotes cannot disagree.
func quoteBudgetInTx(ctx context.Context, tx *sql.Tx, now time.Time, p BudgetParams) (BudgetState, error) {
	// ── the balance, lazily refilled ─────────────────────────────────────
	// Lazy: no timer, no sweeper. A balance is only ever observed through
	// this function, so refilling at read time is indistinguishable from
	// refilling continuously — and it cannot drift when the process is down.
	balance := p.Capacity
	var stored float64
	var updatedAt string
	switch err := tx.QueryRowContext(ctx,
		`SELECT balance, updated_at FROM budgets WHERE agent = ?`, p.Agent).Scan(&stored, &updatedAt); {
	case err == nil:
		prev, perr := time.Parse(tsLayout, updatedAt)
		if perr != nil {
			// An unparseable timestamp is could-not-grade, NOT "no elapsed
			// time". Refusing to guess here fails toward NOT refilling, which
			// is the conservative direction: it can delay a send, never
			// authorise one the budget could not afford.
			balance = stored
		} else {
			balance = budget.Refilled(stored, p.Capacity, p.RefillPerHour, now.Sub(prev))
		}
	case errors.Is(err, sql.ErrNoRows):
		// Never sent before: start full, not in debt.
	default:
		return BudgetState{}, fmt.Errorf("store: read budget: %w", err)
	}

	// ── the window, DERIVED rather than stored ───────────────────────────
	// The set of recipients this sender has already paid breadth for is
	// exactly "distinct to_agent in `messages` within the window". Deriving
	// it means there is no second table to keep in step with `messages`,
	// nothing to reconcile after a crash, and no way for the two to
	// disagree. Refused rows are excluded: a refused message never reached
	// anyone, so charging breadth for it would make a refusal raise the
	// price of the retry.
	// ⚠️ TWO CLOCKS MEET HERE. `now` is Go's; `messages.created_at` is written
	// by SQLite's strftime('now'). Both are UTC wall-clock on the same host, so
	// they agree in production — but a caller that injects a synthetic `now`
	// (tests, replay) is comparing against rows stamped with the REAL clock,
	// and the window silently covers the wrong span. Seed rows with an explicit
	// created_at when driving time by hand.
	cutoff := now.Add(-p.Window).UTC().Format(tsLayout)
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT to_agent FROM messages
		 WHERE from_agent = ? AND created_at > ? AND state != ?`,
		p.Agent, cutoff, StateRefused)
	if err != nil {
		return BudgetState{}, fmt.Errorf("store: budget window: %w", err)
	}
	seen := map[string]struct{}{}
	for rows.Next() {
		var to string
		if err := rows.Scan(&to); err != nil {
			_ = rows.Close()
			return BudgetState{}, fmt.Errorf("store: budget window scan: %w", err)
		}
		seen[to] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return BudgetState{}, fmt.Errorf("store: budget window: %w", err)
	}
	_ = rows.Close()

	prior := len(seen)
	for _, to := range p.Recipients {
		seen[to] = struct{}{}
	}
	newDistinct := len(seen)

	cost := budget.Cost(prior, newDistinct, len(p.Recipients), p.Bytes, p.Alpha)
	st := BudgetState{
		Agent:           p.Agent,
		Balance:         balance,
		Capacity:        p.Capacity,
		PriorRecipients: prior,
		NewRecipients:   newDistinct,
		Deliveries:      len(p.Recipients),
		Bytes:           p.Bytes,
		Cost:            cost,
		Affordable:      budget.Affordable(balance, cost, p.Capacity),
		TimeToAfford:    budget.TimeToAfford(balance, cost, p.Capacity, p.RefillPerHour),
		Remaining:       balance - cost,
	}
	return st, nil
}

// whenAffordable renders the wait a sender can act on.
//
// ⚠️ budget.TimeToAfford returns 0 both when the send is ALREADY affordable
// and when refill is DISABLED (ratePerHour <= 0) — two opposite meanings on
// one value, and the refusal path only ever sees the second. Rendering that
// zero verbatim would print "affordable in 0s" on a budget that will never
// refill: an exit-0-shaped answer inside a refusal, telling the sender to
// retry immediately, forever. Caught by a test that froze the refill rate to
// isolate a race, not by reading the function.
func whenAffordable(d time.Duration, ratePerHour float64) string {
	if ratePerHour <= 0 {
		return "never — refill is disabled (refill_per_hour=0); raise the cap or the rate"
	}
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return "in " + d.Round(time.Second).String()
	default:
		return "in " + d.Round(time.Minute).String()
	}
}

// RefundBudget credits `amount` back to an agent's balance, capped at
// Capacity.
//
// 🔑 IT EXISTS FOR EXACTLY ONE CASE AND MUST NOT GROW A SECOND. A fan-out is
// charged BEFORE the loop, so a send that is charged and then fails for
// EVERY recipient has paid full price and delivered nothing. Zero-landed is
// unambiguous: nobody received it, so nobody should have paid for it.
//
// ⚠️ DO NOT EXTEND THIS TO PARTIAL FAILURES. Refunding 1 of 4 recipients
// means computing what "one recipient's share" of a superlinear breadth term
// is, and there is no principled answer — breadth is a property of the SET,
// not a sum over members. A partial refund would also reintroduce exactly the
// compensating-write complexity that charging-before-the-loop was chosen to
// avoid. Partial delivery keeps the full charge, deliberately, and that is
// stated in the response rather than silently absorbed.
//
// Discovered by a smoke test, not by design: a 4-way fan-out was charged 2.0
// and then refused for all four recipients by the sender-backlog cap. The
// balance had moved and nothing had been delivered.
func (s *Store) RefundBudget(ctx context.Context, now time.Time, agent string, amount, capacity float64) error {
	if amount <= 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: refund budget: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var balance float64
	switch err := tx.QueryRowContext(ctx,
		`SELECT balance FROM budgets WHERE agent = ?`, agent).Scan(&balance); {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		// Nothing was ever charged, so there is nothing to give back. Not an
		// error: a refund for a charge that did not happen is a no-op, and
		// creating a row here would invent a balance.
		return nil
	default:
		return fmt.Errorf("store: refund budget read: %w", err)
	}

	balance += amount
	if balance > capacity {
		balance = capacity
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE budgets SET balance = ?, updated_at = ? WHERE agent = ?`,
		balance, now.UTC().Format(tsLayout), agent); err != nil {
		return fmt.Errorf("store: refund budget write: %w", err)
	}
	return tx.Commit()
}
