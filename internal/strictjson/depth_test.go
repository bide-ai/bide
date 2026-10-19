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

// A document whose structure is nested past MaxDepth is refused, with an error naming the limit and
// the offset of the bracket that crossed it and nothing of the content, in constant memory however
// deep the document is (past MaxVerbatimDepth before any decoder runs).
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
				} {
					err := f()
					if err == nil {
						t.Fatal("a document nested past the limit was accepted")
					}
					msg := err.Error()
					if !strings.Contains(msg, "nesting deeper than 128 levels at offset") && !strings.Contains(msg, "nesting deeper than 10000 levels at offset") {
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

// A journal keeps a tool call's input and a tool's result verbatim, whatever a model or tool made
// them, and a record carrying one must stay readable: json.RawMessage content is held to
// MaxVerbatimDepth, not MaxDepth. Structure around it still is held to MaxDepth.
func TestDepthLimit_VerbatimJSONIsHeldToTheHigherLimit(t *testing.T) {
	type record struct {
		Kind  string          `json:"kind"`
		Input json.RawMessage `json:"input"`
	}
	doc := func(n int) []byte {
		return []byte(`{"kind":"call","input":` + string(deepObjects(n)) + `}`)
	}
	for _, n := range []int{MaxDepth, MaxDepth + 3, 500, 9000, MaxVerbatimDepth - 1} {
		var r record
		if err := Unmarshal(doc(n), &r, nil); err != nil {
			t.Errorf("a record with a verbatim input %d deep: %v", n, err)
		}
	}
	for _, n := range []int{MaxVerbatimDepth, 100_000} {
		d := doc(n)
		bounded(t, 1<<20, func() {
			var r record
			err := Unmarshal(d, &r, nil)
			if err == nil || !strings.Contains(err.Error(), "nesting deeper than 10000 levels at offset") {
				t.Errorf("a record with a verbatim input %d deep: %v, want a refusal naming 10000", n, err)
			}
		})
	}
	// Verbatim levels are not structure: 100 levels of structure around a 9000-deep input is read,
	// and one more level of structure than MaxDepth is not, however shallow the input.
	wrap := func(n int, in string) []byte {
		return []byte(strings.Repeat(`[`, n) + in + strings.Repeat(`]`, n))
	}
	var v any
	if err := Unmarshal(wrap(100, `1`), &v, nil); err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(wrap(MaxDepth+1, `1`), &v, nil); err == nil {
		t.Fatal("structure past MaxDepth accepted")
	}
	type deep struct {
		A []json.RawMessage `json:"a"`
	}
	var d deep
	if err := Unmarshal([]byte(`{"a":[`+string(deepArrays(9000))+`]}`), &d, nil); err != nil {
		t.Fatalf("a verbatim element 9000 deep: %v", err)
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

// allocated is the bytes f allocates, averaged over runs.
func allocated(runs int, f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / uint64(runs)
}

// Check does not rebuild the path to a value at every level: the memory to read a document grows
// in proportion to its depth. Doubling the depth from 64 to 128 at most doubles the allocation
// (a path rebuilt at every level quadruples it), whatever the absolute numbers are.
func TestCheck_PathIsBuiltLazily(t *testing.T) {
	long := strings.Repeat("k", 64)
	nest := func(n int) []byte {
		return []byte(strings.Repeat(`{"`+long+`":`, n) + "1" + strings.Repeat("}", n))
	}
	measure := func(n int) uint64 {
		doc := nest(n)
		return allocated(50, func() {
			if err := Check(doc, reflect.TypeFor[any](), nil); err != nil {
				t.Fatal(err)
			}
		})
	}
	small, big := measure(MaxDepth/2), measure(MaxDepth)
	if ratio := float64(big) / float64(small); ratio > 2.5 {
		t.Fatalf("allocation grew %.1fx (%d to %d bytes) when the depth doubled; want linear growth", ratio, small, big)
	}
}

// 100 KB of brackets, the size that took seconds and gigabytes before the limit, is refused at once
// under either encoding/json.
func TestDepthLimit_LargeInputIsBounded(t *testing.T) {
	doc := deepArrays(50_000)
	bounded(t, 1<<20, func() {
		if err := Check(doc, reflect.TypeFor[any](), nil); err == nil {
			t.Fatal("accepted")
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

// CheckValue, which hooks use on a value inside a document, scans its raw bytes for the verbatim
// limit too.
func TestCheckValue_RefusesDeepVerbatim(t *testing.T) {
	doc := deepArrays(100_000)
	bounded(t, 1<<20, func() {
		err := CheckValue(doc, reflect.TypeFor[json.RawMessage](), "$.x", nil)
		if err == nil || !strings.Contains(err.Error(), "$.x: nesting deeper than 10000 levels at offset") {
			t.Fatalf("CheckValue = %v", err)
		}
	})
}
