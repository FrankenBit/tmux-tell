Feature: Chamber attention signal — flag_operator / register cycle (#224)
  A chamber calls flag_operator to signal it needs operator attention.
  The substrate records awaiting_operator on the chamber's agent row.
  The next register call clears the flag back to idle.

  Scenario: flag_operator sets awaiting_operator; register clears it
    Given agent "alice" is registered
    When "alice" signals attention via flag_operator
    Then "alice"'s attention state is "awaiting_operator"
    When "alice" re-registers
    Then "alice"'s attention state is "idle"
