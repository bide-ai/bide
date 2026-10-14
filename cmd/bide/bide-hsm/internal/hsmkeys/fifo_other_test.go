//go:build !unix

package hsmkeys

import "testing"

// makeFIFO returns "" where there are no named pipes.
func makeFIFO(*testing.T, string) string { return "" }
