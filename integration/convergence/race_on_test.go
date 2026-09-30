//go:build race

package convergence_test

// raceEnabled reports whether the test binary was built with -race. The race detector slows
// memory-heavy tests several times over, so scale tests run smaller under it: a few thousand
// concurrent agents still exercise every shared access the detector checks.
const raceEnabled = true
