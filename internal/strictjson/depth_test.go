package strictjson

import (
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// deepArrays is n nested arrays, the cheapest document to write that is as deep as n.
func deepArrays(n int) []byte {
	return []byte(strings.Repeat("[", n) + strings.Repeat("]", n))
}

// deepObjects is n nested objects.
func deepObjects(n int) []byte {
	return []byte(strings.Repeat(`{"a":`, n) + "1" + strings.Repeat("}", n))
}

// bounded runs f and fails if it allocates more than limit bytes, so a check whose memory grows
// with the square of the nesting depth fails here even though it finishes.
func bounded(t *testing.T, limit uint64, f func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > limit {
		t.Fatalf("allocated %d bytes, want at most %d", got, limit)
	}
}

// A document nested past MaxDepth is refused before any decoder runs, with an error naming the
// limit and the offset of the bracket that crossed it and nothing of the content, in constant
// memory however deep the document is.
func TestDepthLimit_RefusesDeepDocuments(t *testing.T) {
	for name, doc := range map[string][]byte{
		"arrays":     deepArrays(30_000),
		"objects":    deepObjects(10_000),
		"just over":  deepArrays(MaxDepth + 1),
		"in a value": []byte(`{"k":"x","v":` + string(deepArrays(MaxDepth)) + `}`),
	} {
		t.Run(name, func(t *testing.T) {
			bounded(t, 1<<20, func() {
				for _, f := range []func() error{
					func() error { return Check(doc, reflect.TypeFor[any](), nil) },
					func() error { return CheckValue(doc, reflect.TypeFor[any](), "$.x", nil) },
					func() error { var v any; return Unmarshal(doc, &v, nil) },
					func() error { var v json.RawMessage; return Unmarshal(doc, &v, &Options{AllowUnknown: true}) },
				} {
					err := f()
					if err == nil {
						t.Fatal("a document nested past the limit was accepted")
					}
					msg := err.Error()
					if !strings.Contains(msg, "nesting deeper than 128") || !strings.Contains(msg, "offset") {
						t.Fatalf("error %q does not name the depth limit and the offset", msg)
					}
					if strings.Contains(msg, "[[[") || strings.Contains(msg, `"a"`) {
						t.Fatalf("error %q quotes the content", msg)
					}
				}
			})
		})
	}
}

// The limit is exactly MaxDepth: that many levels is read, one more is not, and brackets inside
// strings (with escaped quotes before them) do not count.
func TestDepthLimit_Boundary(t *testing.T) {
	var v any
	if err := Unmarshal(deepArrays(MaxDepth), &v, nil); err != nil {
		t.Fatalf("depth %d refused: %v", MaxDepth, err)
	}
	if err := Unmarshal(deepArrays(MaxDepth+1), &v, nil); err == nil {
		t.Fatalf("depth %d accepted", MaxDepth+1)
	}
	// One array whose string holds 1000 opening brackets, an escaped quote and an escaped backslash.
	if err := Unmarshal([]byte(`["`+strings.Repeat(`[{`, 500)+`\"[[[\\"`+`,"\\\\"]`), &v, nil); err != nil {
		t.Fatalf("brackets inside strings were counted: %v", err)
	}
	// An escaped quote does not end a string: the brackets after it are still text.
	if err := Unmarshal([]byte(`["a\"`+strings.Repeat("[", 500)+`"]`), &v, nil); err != nil {
		t.Fatalf("an escaped quote ended the string: %v", err)
	}
	// Depth is the brackets open at once: any number of siblings is allowed.
	if err := Unmarshal([]byte("["+strings.Repeat("[1],{\"a\":[2]},", 1000)+"0]"), &v, nil); err != nil {
		t.Fatalf("siblings were counted as nesting: %v", err)
	}
	// The error names the offset of the bracket that crossed the limit.
	want := fmt.Sprintf("nesting deeper than %d levels at offset %d", MaxDepth, MaxDepth)
	if err := Check(deepArrays(MaxDepth+1), reflect.TypeFor[any](), nil); err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	// A string that ends in an escaped backslash ends at its closing quote.
	deep := `["\\"` + strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth) + `]`
	if err := Unmarshal([]byte(deep), &v, nil); err == nil {
		t.Fatal("a string ending in an escaped backslash swallowed the brackets after it")
	}
}

// Check does not rebuild the path to a value at every level: reading a legitimately deep document
// takes memory that grows with its size, not with the square of its depth.
func TestCheck_PathIsBuiltLazily(t *testing.T) {
	doc := deepArrays(MaxDepth)
	bounded(t, 256<<10, func() {
		for range 20 {
			if err := Check(doc, reflect.TypeFor[any](), nil); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// An error deep in a document still names the full path to it.
func TestCheck_ErrorPath(t *testing.T) {
	err := Check([]byte(`{"a":[{"b":1,"b":2}]}`), reflect.TypeFor[any](), nil)
	if err == nil || !strings.Contains(err.Error(), `$.a[0]: duplicate name "b"`) {
		t.Fatalf("got %v", err)
	}
}
