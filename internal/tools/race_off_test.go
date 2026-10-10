//go:build !race

package tools

// raceEnabled reports whether the race detector is on; it slows code ~20-30x, so
// wall-clock bounds do not hold under it.
const raceEnabled = false
