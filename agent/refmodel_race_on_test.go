//go:build race

package agent_test

// rmRace reports whether the test binary was built with -race, which slows the reference-model
// tests several times over, so they run fewer scenarios under it.
const rmRace = true
