Feature: Operator routing — "operator" resolves to last-seen chamber (#228)
  Sending to the reserved recipient "operator" routes to the chamber the
  operator is currently (or was most recently) attached to. The substrate
  records the operator's last-seen chamber in a presence slot and resolves
  it at send time.

  Scenario: Send to "operator" routes to the operator's last-seen chamber
    Given agent "alice" is registered at pane "%1"
    And the operator was last seen at chamber "alice"
    When the substrate resolves the "operator" recipient
    Then the resolved target is "alice"
