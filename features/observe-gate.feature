Feature: Observe-gate delivery loop
  The mailman delivers messages when the recipient's input row is quiet.
  Paste-safety gating prevents delivery while the recipient is typing or
  has an open popup — protecting their visible input from corruption.

  Scenario: Message to idle recipient transitions to verified delivered
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
