package theme

import (
	"runtime"
	"strings"
	"testing"
)

// A theme file at the size limit, all brackets, is refused with an error naming the depth limit,
// in bounded memory.
func TestParse_RefusesDeepNesting(t *testing.T) {
	data := []byte(strings.Repeat("[", MaxSize))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, err := Parse(data)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "nesting deeper than") {
		t.Fatalf("Parse = %v, want a nesting depth error", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Fatalf("allocated %d bytes for %d bytes of theme", got, MaxSize)
	}
}
