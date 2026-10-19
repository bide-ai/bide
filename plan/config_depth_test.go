package plan

import (
	"runtime"
	"strings"
	"testing"
)

// A config nested far past any real topology is refused with an error naming the depth limit, in
// bounded time and memory.
func TestInspectConfig_RefusesDeepNesting(t *testing.T) {
	data := []byte(`{"version":1,"x":` + strings.Repeat("[", 20_000))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := InspectConfig(data, nil)
	runtime.ReadMemStats(&after)
	var found bool
	for _, p := range r.Problems {
		found = found || strings.Contains(p.Message, "nesting deeper than")
	}
	if !found {
		t.Fatalf("no nesting depth problem in %+v", r.Problems)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 4<<20 {
		t.Fatalf("allocated %d bytes for 20 KB of config", got)
	}
}
