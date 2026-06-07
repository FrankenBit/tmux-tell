// Package testpin holds the test-helper that wires discipline-pin
// tests to the triage discipline ratified in ADR-0001.
//
// A discipline pin is a test that asserts an architectural commitment
// rather than a behavioral contract. When such a test fails, the
// default response is NOT "fix the assertion until green" — that
// silently erodes the commitment. The triage discipline in §Triage of
// ADR-0001 partitions the diagnosis into:
//
//	(a) implementation regressed — fix the code
//	(b) commitment retracted — file an ADR amendment retracting the pin
//	(c) pin miswrote — fix the assertion AND satisfy (c.1)+(c.2) per ADR
//
// The Triage helper installs a t.Cleanup that surfaces the diagnosis
// pointer on failure, so the discipline is visible at the failure site
// rather than aspirational.
package testpin

import "testing"

// Triage installs a failure-time log line pointing at the ADR-0001
// triage discipline. Call as the first line of each TestPin_ function:
//
//	func TestPin_WireShapeSingleSoT_OmitemptyContract(t *testing.T) {
//	    testpin.Triage(t, "WireShapeSingleSoT",
//	        "wire shape is single source-of-truth — JSON-tag-driven, no manual map construction")
//	    // PIN: wire shape is single source-of-truth ...
//	    // ... test body
//	}
//
// The slug argument is the source-of-truth the CI-enforcement tooling
// (#51) parses — keep it exactly equal to one of the slugs in the
// register in `docs/adr/0001-discipline-pins-as-test-category.md`.
func Triage(t *testing.T, slug, commitment string) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf(`PIN FAILURE [%s] — %s
  Triage per ADR-0001 §Triage before fixing the assertion.
  Diagnoses: (a) implementation regressed / (b) commitment retracted / (c) pin miswrote.
  (c) requires both (c.1) regression-test demonstrating strict improvement AND (c.2) ADR amendment.`, slug, commitment)
	})
}
