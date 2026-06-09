Feature: Deferred delivery — hold until trigger fires (#227)
  A message sent with --deliver-after=<trigger> sits in "deferred" state,
  invisible to the mailman, until flush_deferred fires the matching trigger.
  Deferred messages become queued on trigger and are then picked up normally.

  Scenario: Deferred message is invisible to mailman until trigger fires
    Given agents "alice" and "bob" are registered
    When "alice" sends a "resume"-deferred message to "bob"
    Then the message is in state "deferred"
    And the mailman has no messages to claim for "bob"
    When the "resume" trigger fires for "bob"
    Then the message is in state "queued"
    And the mailman claims the message for "bob"
