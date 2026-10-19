package audit

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// UnmarshalStrict reads signed tree heads, evidence and journal exports, some of them with no size
// limit: a document nested far past any export is refused as malformed, with an error naming the
// depth limit, in bounded memory.
func TestUnmarshalStrict_RefusesDeepNesting(t *testing.T) {
	var before, after runtime.MemStats
	data := []byte(strings.Repeat("[", 20_000))
	var v any
	runtime.GC()
	runtime.ReadMemStats(&before)
	err := UnmarshalStrict(data, &v)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "nesting deeper than") {
		t.Fatalf("UnmarshalStrict = %v, want ErrMalformed naming the nesting depth", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Fatalf("allocated %d bytes for 20 KB of input", got)
	}
}
