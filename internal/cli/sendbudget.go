package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/budget"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// budgetTunables carries the #918 knobs resolved through the ordinary
// config precedence chain (file default → per-agent block → hardcoded).
// Resolved once at the CLI/MCP boundary and passed down, so the store
// never reads config.
type budgetTunables struct {
	Enabled       bool
	Capacity      float64
	RefillPerHour float64
	Alpha         float64
	Window        time.Duration
}

// budgetParamsFor builds the store-level params for a proposed send.
func (b budgetTunables) paramsFor(from string, recipients []string, body string) store.BudgetParams {
	return store.BudgetParams{
		Agent:         from,
		Recipients:    recipients,
		Bytes:         len(body),
		Capacity:      b.Capacity,
		RefillPerHour: b.RefillPerHour,
		Alpha:         b.Alpha,
		Window:        b.Window,
	}
}

// chargeSend is the POLICY half of the budget gate: it decides, charges, and
// returns what happened. It renders nothing and knows about no output shape.
//
// 🔴 THE SPLIT EXISTS BECAUSE THE RENDERING IS THE ONE THING THE TWO SEND
// SURFACES DO NOT SHARE, AND #950 IS WHAT IT COSTS TO WRITE THE POLICY INSIDE
// ONE OF THEM. The CLI writes JSON to a Writer and returns a process exit code;
// the MCP tool returns Go values and an error. With the decision embedded in the
// CLI's rendering, the MCP path could not call it — and did not. Measured on
// `main@6ebbc6b`: `internal/cli/mcp.go` carried ZERO budget references against
// 13 in send.go, so every chamber send went unpriced. The `budgets` table agreed
// from the other side — the six heaviest senders had no row at all while the
// five that did were the five lightest.
//
// ⚠️ AND THE COMMENT ON THE OLD FUNCTION SAID THE OPPOSITE, WITH AUTHORITY: it
// opened "chargeForSend is the single budget gate on the send path" — true of
// the path it could see. A claim of singularity is a claim about the surfaces
// you did NOT enumerate, and it read as settled for three weeks.
//
// # Why it sits here and not inside the fan-out loop
//
// This is the all-or-nothing half of #918 and it is STRUCTURAL, not a check that
// happens to run early.
//
// The fan-out paths insert ONE TRANSACTION PER RECIPIENT (sendOneRecipient →
// InsertMessage, in a loop). A budget check inside that loop would refuse
// partway through: recipients 1..k get the message, k+1..n do not, and the
// sender is told the send failed while half the crew has already read it. That
// is strictly worse than either outcome — a partial fan-out is not a cheaper
// send, it is a DIFFERENT MESSAGE, delivered to a subset nobody chose.
//
// Charging the whole fan-out once, before the loop, makes the refusal
// all-or-nothing by construction: at the point of refusal no row has been
// written, so there is nothing to unwind and no compensating delete to get
// wrong.
//
// ⚠️ The converse is a real and accepted cost: a fan-out that is CHARGED and then
// fails mid-loop for an unrelated reason (recipient queue full, pane gone) has
// spent budget on messages that did not land. That is the safe direction — it
// overcharges rather than overdelivering — but it is a cost, not a free lunch,
// and it is the reason the recipient-cap checks still run per-recipient
// underneath.
func chargeSend(
	ctx context.Context,
	s *store.Store,
	b budgetTunables,
	from string,
	recipients []string,
	body string,
) (charged float64, err error) {
	if !b.Enabled || len(recipients) == 0 {
		return 0, nil
	}
	st, err := s.ChargeBudget(ctx, time.Now(), b.paramsFor(from, recipients, body))
	if err != nil {
		return 0, err
	}
	return st.Cost, nil
}

// budgetRefusalText renders an exhaustion refusal.
//
// Every gate prints what it did NOT check (/srv/CLAUDE.md §Mechanism design).
// This one refuses, so the disclosure rides on the refusal — but it names the
// scope regardless, because the sender's next question is "what do I do
// instead" and two of the three answers are cheaper than waiting.
//
// 🔑 IT IS SHARED RATHER THAN DUPLICATED SO THE TWO SURFACES CANNOT DRIFT IN
// WHAT THEY ADVISE. A refusal a chamber meets through the MCP tool and the same
// refusal met through the CLI are the same refusal; if only one of them explains
// that splitting the send does not help, the other teaches the evasion the cost
// function was rewritten to close.
func budgetRefusalText(err error) string {
	return fmt.Sprintf("%s\n"+
		"  splitting this into separate sends does NOT help — breadth is charged once per\n"+
		"  distinct recipient per window, so N sends to N people cost the same as one send\n"+
		"  to N people. Shortening the body and dropping recipients both do.\n"+
		"  This does NOT check whether the recipients are reachable or their queues have\n"+
		"  room; those are separate refusals you may still hit after this one clears.",
		err.Error())
}

// chargeForSend is the CLI rendering of chargeSend: same decision, written to
// stdout/stderr as JSON with a process exit code. See chargeSend for why the
// decision is not in here.
func chargeForSend(
	ctx context.Context,
	s *store.Store,
	b budgetTunables,
	from string,
	recipients []string,
	body string,
	stdout, stderr io.Writer,
) (charged float64, ok bool, code int) {
	charged, err := chargeSend(ctx, s, b, from, recipients, body)
	switch {
	case err == nil:
		return charged, true, 0
	case errors.Is(err, store.ErrBudgetExhausted):
		return 0, false, writeJSONError(stdout, stderr, budgetRefusalText(err), exitUnavailable)
	default:
		return 0, false, writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
}

// resolveBudgetTunables walks the config precedence chain. Kept beside the
// gate rather than in the flag block so the defaults and the reason for
// each are readable together.
func resolveBudgetTunables(resolveInt func(field string, hardcoded int) int, enabled bool) budgetTunables {
	return budgetTunables{
		Enabled: enabled,
		// ⚠️ CALIBRATION IS PROVISIONAL AND FLAGGED AS SUCH. 500/80 were
		// chosen so a normal working day does not touch the ceiling and a
		// 6-recipient broadcast is noticeable — NOT measured against real
		// traffic. The replay against messages.db is owned separately and
		// had not run when this shipped. Treat a refusal in the field as
		// evidence about the numbers before evidence about the sender.
		Capacity:      float64(resolveInt("budget-cap", int(budget.DefaultCap))),
		RefillPerHour: float64(resolveInt("budget-refill-per-hour", int(budget.DefaultRefillPerHour))),
		// alpha is expressed in hundredths so it goes through the same
		// int-typed precedence chain as every other knob rather than
		// needing a parallel float one.
		Alpha:  float64(resolveInt("budget-alpha-hundredths", int(budget.DefaultAlpha*100))) / 100,
		Window: time.Duration(resolveInt("budget-window-minutes", int(budget.DefaultWindow/time.Minute))) * time.Minute,
	}
}

// BudgetQuote is the --dry-run / `budget` payload.
//
// 🔑 IT REPORTS TWO COSTS ON PURPOSE. The STATIC cost is what this send
// would cost into an empty window — the figure a sender can reason about
// before they know what they have already sent. The WINDOWED cost is what
// they will actually be charged now. They differ by exactly the breadth
// already paid for, and reporting only one of them teaches the wrong
// model: the static figure alone makes repeat sends look expensive when
// they are nearly free, and the windowed figure alone makes a sender who
// has just broadcast conclude that fan-out is cheap.
type BudgetQuote struct {
	OK      bool   `json:"ok"`
	Agent   string `json:"agent"`
	DryRun  bool   `json:"dry_run"`
	Enabled bool   `json:"enabled"`

	Balance   float64 `json:"balance"`
	Capacity  float64 `json:"capacity"`
	Overdraft float64 `json:"overdraft_floor"`

	Recipients      []string `json:"recipients"`
	Deliveries      int      `json:"deliveries"`
	Bytes           int      `json:"bytes"`
	PriorRecipients int      `json:"window_recipients_already_reached"`
	NewRecipients   int      `json:"window_recipients_after"`

	CostWindowed float64 `json:"cost"`
	CostStatic   float64 `json:"cost_into_empty_window"`

	Affordable   bool    `json:"affordable"`
	Remaining    float64 `json:"balance_after"`
	TimeToAfford string  `json:"affordable_when"`
	Note         string  `json:"note,omitempty"`
}

// writeBudgetQuote answers "what would this cost" without charging.
func writeBudgetQuote(ctx context.Context, s *store.Store, p sendParams, recipients []string, stdout, stderr io.Writer) int {
	b := p.Budget
	if !b.Enabled {
		// ⚠️ Affordable is TRUE here, and the reason matters. Every numeric
		// field is zero because nothing was computed — but `affordable:false`
		// beside them would read as "this send is refused", which is the
		// OPPOSITE of what a disabled budget does. The field answers "will
		// this send go through", and with the budget off the answer is yes.
		// Two outcomes must not share a rendering; the note carries the
		// distinction the zeros cannot.
		if err := writeJSONResult(stdout, BudgetQuote{
			OK: true, Agent: p.From, DryRun: true, Enabled: false,
			Recipients:   recipients,
			Deliveries:   len(recipients),
			Bytes:        len(p.Body),
			Affordable:   true,
			TimeToAfford: "now",
			Note:         "the communication budget is DISABLED (budget-enabled=0); nothing was priced and nothing will be refused. The zeros below are unmeasured, not measured-as-zero.",
		}); err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		return exitOK
	}
	now := time.Now()
	st, err := s.QuoteBudget(ctx, now, b.paramsFor(p.From, recipients, p.Body))
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	// The static figure: the same send priced as though the window were
	// empty. Computed from the cost function directly rather than by a
	// second store round-trip, so the two figures cannot disagree about
	// the recipient count or the body length.
	static := budget.Cost(0, len(recipients), len(recipients), len(p.Body), b.Alpha)

	if err := writeJSONResult(stdout, BudgetQuote{
		OK:              true,
		Agent:           p.From,
		DryRun:          p.DryRun,
		Enabled:         true,
		Balance:         round1(st.Balance),
		Capacity:        st.Capacity,
		Overdraft:       round1(budget.Overdraft(st.Capacity)),
		Recipients:      recipients,
		Deliveries:      st.Deliveries,
		Bytes:           st.Bytes,
		PriorRecipients: st.PriorRecipients,
		NewRecipients:   st.NewRecipients,
		CostWindowed:    round1(st.Cost),
		CostStatic:      round1(static),
		Affordable:      st.Affordable,
		Remaining:       round1(st.Remaining),
		TimeToAfford:    whenAffordableCLI(st.TimeToAfford, b.RefillPerHour),
	}); err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	return exitOK
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// whenAffordableCLI mirrors the store's refusal rendering. ⚠️ It must keep
// treating a zero from a DISABLED refill rate as "never" rather than "now"
// — the two are opposite answers on the same value, and only the caller
// knows the rate.
func whenAffordableCLI(d time.Duration, ratePerHour float64) string {
	if ratePerHour <= 0 {
		return "never — refill is disabled"
	}
	if d <= 0 {
		return "now"
	}
	if d < time.Minute {
		return "in " + d.Round(time.Second).String()
	}
	return "in " + d.Round(time.Minute).String()
}

// refundSend credits a fully-failed fan-out back and returns the warning text
// if the credit did not land, or "" if it did.
//
// ⚠️ A refund that itself fails is reported and deliberately NOT made fatal:
// the send has already been answered, and turning a bookkeeping failure into a
// failed send would tell the caller their send failed differently than it did.
// The warning names the AMOUNT so the discrepancy is reconstructable rather
// than merely announced.
//
// 🔑 Split from its rendering for the same reason as chargeSend: the CLI has a
// stderr to write this to and the MCP surface does not, so a version that takes
// an io.Writer is a version one of the two callers cannot use.
func refundSend(ctx context.Context, s *store.Store, p sendParams, amount float64) string {
	if err := s.RefundBudget(ctx, time.Now(), p.From, amount, p.Budget.Capacity); err != nil {
		return fmt.Sprintf("budget refund failed (%.1f not credited back to %s): %v",
			amount, p.From, err)
	}
	return ""
}

// refundSendBudget is the CLI rendering of refundSend.
func refundSendBudget(ctx context.Context, s *store.Store, p sendParams, amount float64, stderr io.Writer) string {
	if w := refundSend(ctx, s, p, amount); w != "" {
		fmt.Fprintln(stderr, "warning: "+w)
		return w
	}
	return ""
}

// warningsFor seeds a response's warning list from the send's config warning.
// Returns nil (not an empty slice) when there is nothing to say, so the field is
// omitted from JSON rather than rendering as an empty array — an empty array
// reads as "checked, nothing found", which is a different claim from "nothing
// to report".
func warningsFor(configWarning string) []string {
	if configWarning == "" {
		return nil
	}
	return []string{configWarning}
}
