//go:build race

package gateway

// raceEnabled reports whether this test binary was built with -race, whose
// instrumentation slows CPU-bound code by roughly an order of magnitude.
const raceEnabled = true
