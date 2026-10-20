package audit

import (
	"bytes"
	"encoding/json"

	"errors"
	"github.com/bide-ai/bide/agent"
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

// A journal keeps a tool call's arguments and a tool's result verbatim, whatever a model or a tool
// made them, so what the journal writer accepts the export reader must read: otherwise a model
// could make its run's export unverifiable. Writer and reader meet at encoding/json's own nesting
// limit (10000, in both encoding/json v1 and v2): the writer refuses a record past it, and the
// reader refuses one past it too, and every depth between is read. This walks the depths around
// that limit and some far below it, and holds the reader to the writer's answer at each.
func TestJournalExport_ReadsWhatTheWriterAccepts(t *testing.T) {
	nest := func(n int) json.RawMessage {
		return json.RawMessage(strings.Repeat("[", n) + strings.Repeat("]", n))
	}
	depths := []int{1, 100, 128, 129, 200, 500, 9000}
	for n := 9985; n <= 10002; n++ {
		depths = append(depths, n)
	}
	var wrote, refused int
	for _, n := range depths {
		records := map[string]agent.Record{
			"tool call": {Name: "t/0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{
				agent.ToolUse{ID: "u1", Name: "x", Args: nest(n)},
			}}},
			"tool result": {Name: "t/1", Kind: agent.StepToolResult, ToolUseID: "u1", Result: nest(n)},
		}
		for name, rec := range records {
			b, err := json.Marshal(rec)
			if err != nil {
				refused++
				continue // the writer refuses it: nothing is journaled
			}
			wrote++
			x := JournalExport{Format: JournalExportFormat, RunID: "r", Records: [][]byte{b}}
			if _, err := x.Journal(); err != nil {
				t.Errorf("the writer accepted a %s nested %d deep, and the export does not read: %v", name, n, err)
			}
		}
	}
	if wrote == 0 || refused == 0 {
		t.Fatalf("the depths straddle no writer limit: %d written, %d refused", wrote, refused)
	}
	// Past the limit by a lot, bytes the writer could not have produced are refused, cheaply.
	rec := agent.Record{Name: "t/1", Kind: agent.StepToolResult, ToolUseID: "u1", Result: json.RawMessage(`{"a":1}`)}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte(`{"a":1}`), []byte(strings.Repeat("[", 100_000)), 1)
	x := JournalExport{Format: JournalExportFormat, RunID: "r", Records: [][]byte{b}}
	if _, err := x.Journal(); !errors.Is(err, ErrMalformed) {
		t.Errorf("a record nested 100000 deep: %v, want ErrMalformed", err)
	}
}

// A type error from the strict reader's final decode names the field, but not 5,000 characters of
// the document.
func TestUnmarshalStrict_TypeErrorIsBounded(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	err := UnmarshalStrict([]byte(`{"a":"`+strings.Repeat("v", 5000)+`"}`), &v)
	if err == nil || len(err.Error()) > 600 || strings.Contains(err.Error(), strings.Repeat("v", 300)) {
		t.Fatalf("UnmarshalStrict = %d bytes: %.200v", len(err.Error()), err)
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("not ErrMalformed: %v", err)
	}
}
