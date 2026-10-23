package agent

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func modelRec(calls ...string) Record { // calls: id, name pairs
	var parts []Part
	for i := 0; i+1 < len(calls); i += 2 {
		parts = append(parts, ToolUse{ID: calls[i], Name: calls[i+1], Args: json.RawMessage(`{}`)})
	}
	return Record{Kind: StepModel, Name: "@llm/x", Message: &Message{Role: RoleAssistant, Parts: parts}}
}

func resRec(id, mark string) Record {
	r := Record{Kind: StepToolResult, ToolUseID: id, Safety: &Safety{}}
	r.Safety.Untrusted = mark
	return r
}

func startRec(untrustedInput string) Record {
	s := RunStart{Input: UserText("go")}
	if untrustedInput != "" {
		s.Settings.UntrustedInput = &untrustedInput
	}
	b, _ := json.Marshal(s)
	return Record{Kind: StepValue, Name: runStartStep, Result: b}
}

func sources(ms []taintMark) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Source)
	}
	return out
}

func marksBefore(t *testing.T, ts []turnTaint, i int) []string {
	t.Helper()
	for _, x := range ts {
		if x.ModelIdx == i {
			return sources(x.Marks)
		}
	}
	t.Fatalf("no turn at %d in %+v", i, ts)
	return nil
}

// T2: a turn's calls are tainted exactly when a mark precedes the requesting model record, and the
// calls of one turn do not taint each other.
func TestFoldTaint_MarksPrecedeTheTurn(t *testing.T) {
	recs := []Record{
		startRec(""),
		modelRec("r", "read", "w", "write"), // 1: a read and a write in one turn
		resRec("r", "crm"),                  // 2
		resRec("w", ""),                     // 3
		modelRec("w2", "write"),             // 4
		resRec("w2", ""),                    // 5
	}
	ts := foldTaint(recs, nil, nil)
	if len(ts) != 2 {
		t.Fatalf("%d turns, want 2", len(ts))
	}
	if got := marksBefore(t, ts, 1); len(got) != 0 {
		t.Errorf("the first turn, whose own read comes after it, is marked %v", got)
	}
	if got := marksBefore(t, ts, 4); !slices.Equal(got, []string{"crm"}) {
		t.Errorf("the second turn is marked %v, want [crm]", got)
	}
	if m, ok := taintOfCall(recs, "w", nil, nil); !ok || len(m) != 0 {
		t.Errorf("the write requested beside the read: %v %v, want untainted", m, ok)
	}
	if m, ok := taintOfCall(recs, "w2", nil, nil); !ok || len(m) != 1 || m[0].Source != "crm" || m[0].Pos != 2 || m[0].Ref != "tool:read/r" {
		t.Errorf("the later write: %+v %v, want the crm mark at 2 from tool:read/r", m, ok)
	}
	if _, ok := taintOfCall(recs, "nope", nil, nil); ok {
		t.Error("a call no turn requested has a turn")
	}
}

func TestFoldTaint_InputRetrievalSagaFailAndInherited(t *testing.T) {
	ret := Record{Kind: StepValue, Name: "@retrieval/0", Result: json.RawMessage(`{"query":"q","docs":[],"untrusted":"kb"}`)}
	plainRet := Record{Kind: StepValue, Name: "@retrieval/1", Result: json.RawMessage(`{"query":"q","docs":[]}`)}
	fail := resRec("f", "web")
	fail.Kind = StepSagaFail
	cases := []struct {
		name      string
		inherited []taintMark
		recs      []Record
		want      []string
	}{
		{"untrusted input", nil, []Record{startRec("inbound-email"), modelRec()}, []string{"inbound-email"}},
		{"trusted input", nil, []Record{startRec(""), modelRec()}, nil},
		{"retrieval mark", nil, []Record{startRec(""), ret, plainRet, modelRec()}, []string{"kb"}},
		{"inherited marks come first", []taintMark{{Source: "parent"}}, []Record{startRec("in"), modelRec()}, []string{"parent", "in"}},
		{"a saga_fail is a result", nil, []Record{startRec(""), modelRec("f", "get"), fail, modelRec()}, []string{"web"}},
		{"a result with no safety is unmarked", nil, []Record{startRec(""), modelRec("a", "t"), {Kind: StepToolResult, ToolUseID: "a"}, modelRec()}, nil},
	}
	for _, c := range cases {
		ts := foldTaint(c.recs, c.inherited, nil)
		got := sources(ts[len(ts)-1].Marks)
		if !slices.Equal(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			t.Errorf("%s: marks %v, want %v", c.name, got, c.want)
		}
	}
	if got := foldTaint([]Record{modelRec()}, nil, nil); len(got) != 1 || len(got[0].Marks) != 0 {
		t.Errorf("a turn with no mark: %+v", got)
	}
}

// The larger of the recorded and the current declaration, in the monotone direction: a result of a tool
// declared Untrusted now is marked though its record carries none; a recorded mark is never lowered.
func TestFoldTaint_DeclaredIsOnlyEverAdded(t *testing.T) {
	recs := []Record{
		startRec(""),
		modelRec("a", "read", "b", "other"),
		resRec("a", ""),         // recorded unmarked; the tool is declared now
		resRec("b", "recorded"), // recorded marked; no longer declared
		modelRec(),
	}
	declared := func(tool string) string {
		if tool == "read" {
			return "now"
		}
		return ""
	}
	if got := marksBefore(t, foldTaint(recs, nil, declared), 4); !slices.Equal(got, []string{"now", "recorded"}) {
		t.Errorf("with the declaration: %v, want [now recorded]", got)
	}
	if got := marksBefore(t, foldTaint(recs, nil, nil), 4); !slices.Equal(got, []string{"recorded"}) {
		t.Errorf("without it: %v, want [recorded] (a recorded mark stays; nothing is added)", got)
	}
	other := func(string) string { return "different" }
	if got := marksBefore(t, foldTaint(recs, nil, other), 4); !slices.Contains(got, "recorded") {
		t.Errorf("a declaration of another source lowered the recorded mark: %v", got)
	}
}

// T6, the fold's half: the same records give the same result, and the fold does not modify them.
func TestFoldTaint_IsPure(t *testing.T) {
	recs := []Record{modelRec("a", "read"), resRec("a", "crm"), startRec("in"), modelRec()}
	before, _ := json.Marshal(recs)
	a := foldTaint(recs, []taintMark{{Source: "p"}}, nil)
	b := foldTaint(recs, []taintMark{{Source: "p"}}, nil)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("two folds differ: %+v vs %+v", a, b)
	}
	after, _ := json.Marshal(recs)
	if string(before) != string(after) {
		t.Error("the fold modified its records")
	}
	// a caller that appends to an earlier turn's marks does not change a later turn's: with an
	// inherited mark and two results before the second turn, its marks have spare capacity that a
	// shared slice would let the append write into the fourth mark of the third turn
	grow := []Record{modelRec("a", "t", "b", "t", "c", "t"), resRec("a", "x"), resRec("b", "y"), modelRec(), resRec("c", "z"), modelRec()}
	a = foldTaint(grow, []taintMark{{Source: "p"}}, nil)
	first, last := a[1], a[2]
	want := slices.Clone(sources(last.Marks))
	first.Marks = append(first.Marks, taintMark{Source: "scribble"})
	if len(last.Marks) != 4 {
		t.Fatalf("the third turn has %d marks, want 4", len(last.Marks))
	}
	if got := sources(last.Marks); !slices.Equal(got, want) {
		t.Errorf("a change to an earlier turn's marks reached a later turn's: %v, was %v", got, want)
	}
}
