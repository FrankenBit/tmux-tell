//go:build race

package healthscan

// raceDetector is true when the test binary was built with -race.
// Used by pin_test.go to skip wall-clock assertions that are unreliable
// under race-detector overhead. See ADR-0001 §Amendment-2026-06-09, #254.
const raceDetector = true
