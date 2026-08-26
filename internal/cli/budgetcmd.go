package cli

import (
	"context"
	"flag"
	"io"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// runBudgetCLI reports an agent's communication budget (#918).
//
// With no --to it answers "what do I have"; with --to it answers "what would
// this send cost", which is the same code path as `send --dry-run` — the
// quote is computed in ONE place (writeBudgetQuote → store.QuoteBudget) so
// the figure this verb prints and the figure a refusal quotes cannot drift.
func runBudgetCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("budget", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	from := fs.String("from", "", "agent whose budget to read (env: TMUX_AGENT_NAME)")
	to := fs.String("to", "", "optional: price a hypothetical send to these recipients (comma-separated)")
	body := fs.String("body", "", "optional: body to price; length is part of the cost")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	ctx := context.Background()
	fromName, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	cfg, _ := config.Load()
	budgetOn := config.ResolveInt(cfg, fromName, "budget-enabled", 0) != 0
	tunables := resolveBudgetTunables(func(field string, hardcoded int) int {
		return config.ResolveInt(cfg, fromName, field, hardcoded)
	}, budgetOn)

	recipients := splitRecipients(*to)
	// No --to: price a zero-recipient send. Cost is 0 by construction
	// (budget.Cost returns 0 for deliveries <= 0), so the quote degenerates
	// to a balance read — which is exactly what "what do I have" means, and
	// it reuses the same reader rather than adding a second one that could
	// report a differently-refilled balance.
	p := sendParams{From: fromName, Body: *body, Budget: tunables, DryRun: true}
	return writeBudgetQuote(ctx, s, p, recipients, stdout, stderr)
}
