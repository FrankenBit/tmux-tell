//go:build !race

package healthscan

// raceDetector is false when the test binary was built without -race.
const raceDetector = false
