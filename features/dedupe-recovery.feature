Feature: Dedupe recovery — delivered_in_input_box replay (#157)
  When a delivery lands in the recipient's input box without verify-token
  confirmation, a resend within the dedupe window is absorbed: the substrate
  finds the prior unverified delivery and upgrades it to verified rather than
  delivering a duplicate.

  Scenario: Resend within dedupe window upgrades original to verified
    Given agents "alice" and "bob" are registered
    And a prior delivery from "alice" to "bob" landed in the input box unverified
    When "alice" resends the same message to "bob"
    And the substrate finds a dedupe match for the resent message
    Then the original delivery is upgraded to verified
