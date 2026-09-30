//go:build !race

package agent_test

// rmRace reports whether the test binary was built with -race (see refmodel_race_on_test.go).
const rmRace = false
