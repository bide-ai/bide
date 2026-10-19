package agent

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// Tool arguments are model-controlled: arguments nested far past anything a tool takes are refused
// with an error naming the depth limit, in bounded time and memory.
func TestDecodeArgs_RefusesDeepNesting(t *testing.T) {
	var before, after runtime.MemStats
	args := json.RawMessage(strings.Repeat("[", 20_000))
	var v struct {
		X any `json:"x"`
	}
	runtime.GC()
	runtime.ReadMemStats(&before)
	err := decodeArgs(args, &v)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "nesting deeper than") {
		t.Fatalf("decodeArgs = %v, want a nesting depth error", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Fatalf("allocated %d bytes for 20 KB of arguments", got)
	}
}
