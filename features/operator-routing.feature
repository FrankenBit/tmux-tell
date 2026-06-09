Feature: Operator routing — presence slot store contract (#228)
  The store's presence slot records the last-seen chamber (SetPresence) and
  is read at send time (GetPresence + GetAgent) to resolve "operator" →
  chamber. This scenario verifies the store-layer contract: the slot records
  and retrieves a registered chamber name. The full resolveOperatorTarget
  chain (tmux list-clients → active-pane → registered-agent check → fallback
  to slot) is tested in cmd/tmux-msg-claude.

  Scenario: Store contract: operator presence slot resolves to a registered chamber
    Given agent "alice" is registered at pane "%1"
    And the operator was last seen at chamber "alice"
    When the substrate resolves the "operator" recipient
    Then the resolved target is "alice"
