//go:build linux || darwin

package runview

import (
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// TestShownRunIDWalkStopsEarly pins the bound on the work of shownRunID: it walks at most
// maxRunIDShown+1 units of the ID and reads nothing past them. The ID below is maxRunIDShown+1 bytes
// at the end of a readable page, and its length runs on into a PROT_NONE page. A walk that does not
// stop (no bound, or one more unit) reads the guard page and faults, which kills the test binary:
// that fault is the failure signal, since the output and the allocations of such a walk are the same
// as the bounded one's and no assertion can see them. The test skips where mmap or mprotect is not
// available. It is built on linux and darwin only: syscall.Mprotect is not defined on the other unix
// systems.
func TestShownRunIDWalkStopsEarly(t *testing.T) {
	page := syscall.Getpagesize()
	mem, err := syscall.Mmap(-1, 0, 2*page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Skipf("mmap: %v", err)
	}
	defer syscall.Munmap(mem)
	if err := syscall.Mprotect(mem[page:], syscall.PROT_NONE); err != nil {
		t.Skipf("mprotect: %v", err)
	}
	start := page - (maxRunIDShown + 1)
	copy(mem[start:page], strings.Repeat("r", maxRunIDShown+1))
	// The string's length reaches page bytes into the guard page; only its first maxRunIDShown+1
	// bytes are readable, and a correct walk reads no more.
	id := unsafe.String(&mem[start], maxRunIDShown+1+page)
	want := `"` + strings.Repeat("r", maxRunIDShown) + `"...`
	if got := shownRunID(id); got != want {
		t.Errorf("shownRunID of a run ID that runs into a guard page = %s, want %s", got, want)
	}
}
