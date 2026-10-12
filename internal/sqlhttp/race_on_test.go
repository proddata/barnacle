//go:build race

package sqlhttp

// The race detector enlarges live heap, so calibration comparisons allow for it.
const raceEnabled = true
