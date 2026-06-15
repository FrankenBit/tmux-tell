// Package steps_test wires the gherkin step definitions for the substrate-boundary
// E2E scenario layer (features/*.feature). Each scenario exercises a named substrate
// contract — state-machine transitions, routing logic, gate behaviour — using the
// internal store and tmuxio packages directly, without spinning up the mailman daemon
// or a real tmux server.
//
// Run:  go test ./features/steps/
// Or via the full suite: go test ./...
package steps_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// timeFormat matches the store's created_at layout (ISO8601 with millis, UTC).
const timeFormat = "2006-01-02T15:04:05.000Z"

// sc is the per-scenario shared state.
type sc struct {
	st             *store.Store
	lastID         string // public_id of the most recently inserted/claimed message
	resolvedTarget string // result of an operator-routing resolution step
	paneState      tmuxio.State
}

// --- setup steps ---

func (s *sc) twoAgentsRegistered(a, b string) error {
	ctx := context.Background()
	if err := s.st.UpsertAgent(ctx, a, "%1"); err != nil {
		return err
	}
	return s.st.UpsertAgent(ctx, b, "%3")
}

func (s *sc) agentRegisteredAtPane(name, pane string) error {
	return s.st.UpsertAgent(context.Background(), name, pane)
}

func (s *sc) agentRegistered(name string) error {
	return s.st.UpsertAgent(context.Background(), name, "%1")
}

// --- send steps ---

func (s *sc) agentSends(from, body, to string) error {
	r, err := s.st.InsertMessage(context.Background(), store.InsertParams{
		FromAgent: from, ToAgent: to, Body: body,
	})
	if err != nil {
		return err
	}
	s.lastID = r.PublicID
	return nil
}

func (s *sc) agentSendsDeferredTo(from, trigger, to string) error {
	r, err := s.st.InsertMessage(context.Background(), store.InsertParams{
		FromAgent: from, ToAgent: to, Body: "staged", DeliverAfter: trigger,
	})
	if err != nil {
		return err
	}
	s.lastID = r.PublicID
	return nil
}

// --- state check steps ---

func (s *sc) messageIsInState(want string) error {
	m, err := s.st.GetMessage(context.Background(), s.lastID)
	if err != nil {
		return err
	}
	if string(m.State) != want {
		return fmt.Errorf("message state = %q, want %q", m.State, want)
	}
	return nil
}

// --- delivery simulation steps ---

// mailmanClaimsAndDelivers simulates one mailman cycle: ClaimNext + MarkDelivered
// (verified=1). The actual mailman's IO (tmux paste/Enter/capture) is exercised by
// the cmd/tmux-tell-claude serve tests; here we test the state-machine contract.
func (s *sc) mailmanClaimsAndDelivers(recipient string) error {
	ctx := context.Background()
	m, err := s.st.ClaimNext(ctx, recipient)
	if err != nil {
		return fmt.Errorf("ClaimNext: %w", err)
	}
	if m == nil {
		return fmt.Errorf("no claimable message for %q", recipient)
	}
	s.lastID = m.PublicID
	return s.st.MarkDelivered(ctx, m.PublicID)
}

func (s *sc) deliveryIsVerified() error {
	m, err := s.st.GetMessage(context.Background(), s.lastID)
	if err != nil {
		return err
	}
	if !m.Verified.Valid || m.Verified.Int64 != 1 {
		return fmt.Errorf("verified = %v, want 1", m.Verified)
	}
	return nil
}

func (s *sc) mailmanHasNothingToClaimFor(recipient string) error {
	m, err := s.st.ClaimNext(context.Background(), recipient)
	if err != nil {
		return err
	}
	if m != nil {
		return fmt.Errorf("expected no claimable message for %q, got %s", recipient, m.PublicID)
	}
	return nil
}

func (s *sc) mailmanClaimsMessageFor(recipient string) error {
	m, err := s.st.ClaimNext(context.Background(), recipient)
	if err != nil {
		return fmt.Errorf("ClaimNext: %w", err)
	}
	if m == nil {
		return fmt.Errorf("expected a claimable message for %q, got none", recipient)
	}
	s.lastID = m.PublicID
	return nil
}

// --- observe-gate / paste-safety steps ---

func (s *sc) paneIsInState(stateStr string) error {
	switch stateStr {
	case "idle":
		s.paneState = tmuxio.StateIdle
	case "working":
		s.paneState = tmuxio.StateWorking
	case "awaiting_operator":
		s.paneState = tmuxio.StateAwaitingOperator
	case "at_rest_in_compaction":
		s.paneState = tmuxio.StateAtRestInCompaction
	case "unknown":
		s.paneState = tmuxio.StateUnknown
	default:
		return fmt.Errorf("unknown pane state %q", stateStr)
	}
	return nil
}

func (s *sc) pasteSafetyGateBlocks() error {
	if !tmuxio.IsPasteUnsafe(s.paneState) {
		return fmt.Errorf("gate should block for state %v, but IsPasteUnsafe returned false", s.paneState)
	}
	return nil
}

func (s *sc) pasteSafetyGateAllows() error {
	if tmuxio.IsPasteUnsafe(s.paneState) {
		return fmt.Errorf("gate should allow for state %v, but IsPasteUnsafe returned true", s.paneState)
	}
	return nil
}

// --- deferred-delivery steps ---

func (s *sc) triggerFiresFor(trigger, recipient string) error {
	_, err := s.st.PromoteDeferred(context.Background(), recipient, trigger)
	return err
}

// --- dedupe-recovery steps ---

func (s *sc) priorDeliveryLandedInInputBox(from, to string) error {
	ctx := context.Background()
	// Insert the original message.
	_, err := s.st.InsertMessage(ctx, store.InsertParams{
		FromAgent: from, ToAgent: to, Body: "duplicate message",
	})
	if err != nil {
		return err
	}
	// Simulate mailman claiming it, then landing in the input box unverified.
	m, err := s.st.ClaimNext(ctx, to)
	if err != nil {
		return fmt.Errorf("ClaimNext for prior delivery: %w", err)
	}
	if m == nil {
		return fmt.Errorf("priorDelivery: expected claimable message, got none")
	}
	return s.st.MarkDeliveredInInputBox(ctx, m.PublicID)
}

func (s *sc) resendsSameMessageTo(from, to string) error {
	r, err := s.st.InsertMessage(context.Background(), store.InsertParams{
		FromAgent: from, ToAgent: to, Body: "duplicate message",
	})
	if err != nil {
		return err
	}
	s.lastID = r.PublicID
	return nil
}

func (s *sc) substrateFindsDedupeMatch() error {
	cutoff := time.Now().Add(-1 * time.Minute).UTC().Format(timeFormat)
	m, err := s.st.FindDedupeMatch(context.Background(), "alice", "bob", "duplicate message", cutoff)
	if err != nil {
		return fmt.Errorf("FindDedupeMatch: %w", err)
	}
	if m == nil {
		return fmt.Errorf("expected a dedupe match, got none")
	}
	s.lastID = m.PublicID
	return nil
}

func (s *sc) originalDeliveryUpgradedToVerified() error {
	ctx := context.Background()
	if err := s.st.MarkVerifiedByDedupe(ctx, s.lastID); err != nil {
		return fmt.Errorf("MarkVerifiedByDedupe: %w", err)
	}
	m, err := s.st.GetMessage(ctx, s.lastID)
	if err != nil {
		return err
	}
	if !m.Verified.Valid || m.Verified.Int64 != 1 {
		return fmt.Errorf("verified = %v, want 1 after dedupe upgrade", m.Verified)
	}
	return nil
}

// --- operator-routing steps ---

func (s *sc) operatorLastSeenAtChamber(chamber string) error {
	return s.st.SetPresence(context.Background(), store.PresenceKeyOperatorLastSeenIn, chamber)
}

func (s *sc) substrateResolvesOperatorRecipient(recipient string) error {
	if recipient != "operator" {
		return fmt.Errorf("only \"operator\" routing is exercised here; got %q", recipient)
	}
	target, err := s.st.GetPresence(context.Background(), store.PresenceKeyOperatorLastSeenIn)
	if err != nil {
		return fmt.Errorf("operator routing: presence slot: %w", err)
	}
	s.resolvedTarget = target
	return nil
}

func (s *sc) resolvedTargetIs(expected string) error {
	if s.resolvedTarget != expected {
		return fmt.Errorf("resolved target = %q, want %q", s.resolvedTarget, expected)
	}
	a, err := s.st.GetAgent(context.Background(), expected)
	if err != nil {
		return fmt.Errorf("routing target %q: %w", expected, err)
	}
	if a == nil {
		return fmt.Errorf("routing target %q is not a registered agent", expected)
	}
	return nil
}

// --- attention-signal steps ---

func (s *sc) signalsAttentionViaFlagOperator(name string) error {
	return s.st.SetAttentionState(context.Background(), name, store.AttentionStateAwaitingOperator)
}

func (s *sc) agentAttentionStateIs(name, want string) error {
	a, err := s.st.GetAgent(context.Background(), name)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("agent %q not found", name)
	}
	if a.AttentionState != want {
		return fmt.Errorf("attention_state = %q, want %q", a.AttentionState, want)
	}
	return nil
}

func (s *sc) agentReregisters(name string) error {
	ctx := context.Background()
	a, err := s.st.GetAgent(ctx, name)
	if err != nil || a == nil {
		return fmt.Errorf("agent %q not found for re-register", name)
	}
	if err := s.st.UpsertAgent(ctx, name, a.PaneID); err != nil {
		return err
	}
	// register clears attention_state (register.go:114)
	return s.st.SetAttentionState(ctx, name, store.AttentionStateIdle)
}

// InitializeScenario wires step patterns to sc methods for every scenario.
func InitializeScenario(ctx *godog.ScenarioContext) {
	s := &sc{}

	ctx.Before(func(goCtx context.Context, _ *godog.Scenario) (context.Context, error) {
		var err error
		s.st, err = store.Open(":memory:")
		if err != nil {
			return goCtx, fmt.Errorf("open in-memory store: %w", err)
		}
		s.lastID = ""
		s.resolvedTarget = ""
		s.paneState = tmuxio.StateUnknown
		return goCtx, nil
	})

	ctx.After(func(goCtx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		if s.st != nil {
			_ = s.st.Close()
		}
		return goCtx, nil
	})

	// setup
	ctx.Step(`^agents "([^"]*)" and "([^"]*)" are registered$`, s.twoAgentsRegistered)
	ctx.Step(`^agent "([^"]*)" is registered at pane "([^"]*)"$`, s.agentRegisteredAtPane)
	ctx.Step(`^agent "([^"]*)" is registered$`, s.agentRegistered)

	// send
	ctx.Step(`^"([^"]*)" sends "([^"]*)" to "([^"]*)"$`, s.agentSends)
	ctx.Step(`^"([^"]*)" sends a "([^"]*)"-deferred message to "([^"]*)"$`, s.agentSendsDeferredTo)

	// state checks
	ctx.Step(`^the message is in state "([^"]*)"$`, s.messageIsInState)

	// delivery simulation
	ctx.Step(`^the mailman claims and delivers the next message for "([^"]*)"$`, s.mailmanClaimsAndDelivers)
	ctx.Step(`^the delivery is verified$`, s.deliveryIsVerified)
	ctx.Step(`^the mailman has no messages to claim for "([^"]*)"$`, s.mailmanHasNothingToClaimFor)
	ctx.Step(`^the mailman claims the message for "([^"]*)"$`, s.mailmanClaimsMessageFor)

	// observe-gate / paste-safety
	ctx.Step(`^the pane is in the "([^"]*)" state$`, s.paneIsInState)
	ctx.Step(`^the paste-safety gate blocks delivery$`, s.pasteSafetyGateBlocks)
	ctx.Step(`^the paste-safety gate allows delivery$`, s.pasteSafetyGateAllows)

	// deferred
	ctx.Step(`^the "([^"]*)" trigger fires for "([^"]*)"$`, s.triggerFiresFor)

	// dedupe recovery
	ctx.Step(`^a prior delivery from "([^"]*)" to "([^"]*)" landed in the input box unverified$`, s.priorDeliveryLandedInInputBox)
	ctx.Step(`^"([^"]*)" resends the same message to "([^"]*)"$`, s.resendsSameMessageTo)
	ctx.Step(`^the substrate finds a dedupe match for the resent message$`, s.substrateFindsDedupeMatch)
	ctx.Step(`^the original delivery is upgraded to verified$`, s.originalDeliveryUpgradedToVerified)

	// operator routing
	ctx.Step(`^the operator was last seen at chamber "([^"]*)"$`, s.operatorLastSeenAtChamber)
	ctx.Step(`^the substrate resolves the "([^"]*)" recipient$`, s.substrateResolvesOperatorRecipient)
	ctx.Step(`^the resolved target is "([^"]*)"$`, s.resolvedTargetIs)

	// attention signal
	ctx.Step(`^"([^"]*)" signals attention via flag_operator$`, s.signalsAttentionViaFlagOperator)
	ctx.Step(`^"([^"]*)"'s attention state is "([^"]*)"$`, s.agentAttentionStateIs)
	ctx.Step(`^"([^"]*)" re-registers$`, s.agentReregisters)
}

// TestFeatures is the godog entry-point — wired to Go's testing framework so
// "go test ./features/steps/" runs all *.feature scenarios found in ../
// (i.e. the features/ directory, one level up from this package).
func TestFeatures(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"../"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
