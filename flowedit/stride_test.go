//go:build !race

package flowedit

// opStride: see stride_race_test.go. Without the race detector every operation is measured.
const opStride = 1
