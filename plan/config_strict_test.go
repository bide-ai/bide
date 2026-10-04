package plan

import (
	"context"
	"testing"
)

// strictRegistry registers two int steps and an always-true int predicate.
func strictRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, name := range []string{"a", "b"} {
		if err := reg.RegisterStep(name, func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly()); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if err := reg.RegisterPredicate("again", func(n int) bool { return n < 0 }); err != nil {
		t.Fatalf("register predicate: %v", err)
	}
	return reg
}

// A config is read as written. encoding/json ignores an unknown or misspelled name, matches names
// case-insensitively, and keeps the last of two duplicates, so a typo silently drops what the
// author wrote: "aproval" loads the node with no gate, and "entrypoint" falls back to the first
// node. Each is a load error.
func TestLoad_ConfigIsReadAsWritten(t *testing.T) {
	for name, cfg := range map[string]string{
		"misspelled approval": `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","aproval":{"need":1,"approvers":["ops"]}},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`,
		"misspelled entry":    `{"version":1,"flow":"f","entrypoint":"b","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`,
		"case variant name":   `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","Safety":"readonly"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`,
		"duplicate name":      `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","safety":"readonly","safety":"idempotent"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`,
		"unknown top level":   `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}],"author":"ops"}`,
		"trailing data":       `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]} {}`,
	} {
		if _, err := Load[int, int]([]byte(cfg), strictRegistry(t)); err == nil {
			t.Errorf("%s: Load accepted the config", name)
		}
		if err := Validate([]byte(cfg), strictRegistry(t)); err == nil {
			t.Errorf("%s: Validate accepted the config", name)
		}
	}
	ok := `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a","safety":"readonly"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`
	if _, err := Load[int, int]([]byte(ok), strictRegistry(t)); err != nil {
		t.Fatalf("Load of a well-formed config: %v", err)
	}
}
