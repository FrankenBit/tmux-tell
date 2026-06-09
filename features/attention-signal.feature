Feature: Chamber attention signal — store state-machine contract (#224)
  flag_operator transitions a chamber's attention_state to awaiting_operator
  via SetAttentionState. Re-registering clears the flag to idle; the register
  handler (register.go:114) calls the same SetAttentionState(idle) store
  primitive directly. This scenario verifies the store-layer contract; the
  full handler chains (doFlagOperator + register CLI path) are tested in
  cmd/tmux-msg-claude.

  Scenario: Store contract: attention state set to awaiting_operator is cleared to idle on re-register
    Given agent "alice" is registered
    When "alice" signals attention via flag_operator
    Then "alice"'s attention state is "awaiting_operator"
    When "alice" re-registers
    Then "alice"'s attention state is "idle"
