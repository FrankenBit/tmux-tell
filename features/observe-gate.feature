Feature: Observe-gate delivery loop — store state-machine contracts
  Scenario 1 verifies the store-layer state machine: queued → claimed →
  delivered+verified. The actual gate check (observe input-row quiet before
  claiming) and mailman IO (paste, Enter, verify-token) are tested in
  cmd/tmux-msg-claude/serve*_test.go; this scenario proves the state
  transitions the mailman produces in the store.

  Scenario 2 directly exercises tmuxio.IsPasteUnsafe — the paste-safety
  gate function the mailman calls before delivery. It is the one scenario
  here that exercises gate code rather than simulating it.

  Scenario: Store contract: queued message is claimed and transitions to verified delivered
    Given agents "alice" and "bob" are registered
    When "alice" sends "hello from alice" to "bob"
    Then the message is in state "queued"
    When the mailman claims and delivers the next message for "bob"
    Then the message is in state "delivered"
    And the delivery is verified

  Scenario: Paste-safety gate blocks delivery while input row is busy
    Given the pane is in the "awaiting_operator" state
    Then the paste-safety gate blocks delivery
    Given the pane is in the "idle" state
    Then the paste-safety gate allows delivery
