package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
)

func taintReason(source string, pos int) gateReason {
	return gateReason{Kind: gateReasonTaint, Source: source, Ref: "read_email/c3", Pos: pos}
}

func capReason(rule string) gateReason { return gateReason{Kind: gateReasonCap, Rule: rule} }

// unionReasons only adds: whatever a record holds, the recomputed reasons stay, and whatever the
// recomputed set holds, the recorded ones stay. Empty or junk records remove nothing.
func TestUnionReasons_OnlyAdds(t *testing.T) {
	recomputed := []gateReason{taintReason("crm", 4), capReason("per_day")}
	cases := map[string][]gateReason{
		"empty record":                nil,
		"same":                        slices.Clone(recomputed),
		"extra":                       {capReason("total")},
		"same source, other position": {taintReason("crm", 5)},
		"junk kind":                   {{Kind: "waive", Rule: "per_day"}},
		"duplicate of both":           {taintReason("crm", 4), capReason("per_day"), capReason("per_day")},
	}
	for name, recorded := range cases {
		got := unionReasons(recomputed, recorded)
		for _, want := range append(slices.Clone(recomputed), recorded...) {
			if !slices.Contains(got, want) {
				t.Errorf("%s: union lost %+v; got %+v", name, want, got)
			}
		}
		if !slices.Equal(got, unionReasons(recorded, recomputed)) {
			t.Errorf("%s: union depends on the argument order", name)
		}
		if again := unionReasons(got, recorded); !slices.Equal(got, again) {
			t.Errorf("%s: union is not idempotent: %+v then %+v", name, got, again)
		}
		seen := map[string]bool{}
		for _, r := range got {
			if seen[r.key()] {
				t.Errorf("%s: duplicate reason %+v", name, r)
			}
			seen[r.key()] = true
		}
	}
	if got := unionReasons(nil, nil); len(got) != 0 {
		t.Fatalf("union of nothing = %+v", got)
	}
}

// A record's Exempt text is evidence for an audit and never an input to the union: it is not a reason,
// and no field of the union's arguments can carry it.
func TestGateReasonRecord_ExemptIsNeverRead(t *testing.T) {
	rec := gateReasonRecord{Format: gateReasonFormat, ToolUseID: "c1", Exempt: "library tool, reviewed"}
	if got := unionReasons([]gateReason{taintReason("crm", 1)}, rec.Reasons); len(got) != 1 {
		t.Fatalf("an exempt record removed or added a reason: %+v", got)
	}
}

func TestGateReason_RoundTripAndStrictRead(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	want := gateReasonRecord{Format: gateReasonFormat, ToolUseID: "c/1#x", Reasons: []gateReason{taintReason("crm", 4), capReason("per_day")},
		PolicyDigest: "ab12", RecordedAt: "2026-10-22T09:00:00.000Z"}
	got, inserted, err := putGateReason(ctx, j, "r", want)
	if err != nil || !inserted {
		t.Fatalf("putGateReason = %+v, %v, %v", got, inserted, err)
	}
	read, ok, err := getGateReason(ctx, j, "r", "c/1#x")
	if err != nil || !ok || read.PolicyDigest != "ab12" || len(read.Reasons) != 2 {
		t.Fatalf("getGateReason = %+v, %v, %v", read, ok, err)
	}
	if _, ok, err := getGateReason(ctx, j, "r", "other"); ok || err != nil {
		t.Fatalf("a call with no record: ok %v, err %v", ok, err)
	}
	if gateReasonStep("c/1#x") != "gate-reason:c%2F1%23x" {
		t.Fatalf("gateReasonStep = %q, want the call ID encoded", gateReasonStep("c/1#x"))
	}
	for name, raw := range map[string]string{
		"not json":                  `"x"`,
		"wrong format":              `{"format":"other","tool_use_id":"c9"}`,
		"another call":              `{"format":"` + gateReasonFormat + `","tool_use_id":"zzz"}`,
		"duplicate name":            `{"format":"` + gateReasonFormat + `","tool_use_id":"c9","tool_use_id":"c9"}`,
		"unknown top-level field":   `{"format":"` + gateReasonFormat + `","tool_use_id":"c9","extra":1}`,
		"unknown field in a reason": `{"format":"` + gateReasonFormat + `","tool_use_id":"c9","reasons":[{"kind":"taint","scope":"x"}]}`,
		"case variant name":         `{"format":"` + gateReasonFormat + `","tool_use_id":"c9","Reasons":[]}`,
	} {
		if _, err := j.put(ctx, "r", gateReasonStep("c9"), Record{Kind: StepValue, Result: json.RawMessage(raw)}); err != nil {
			t.Fatal(err)
		}
		_, ok, err := getGateReason(ctx, j, "r", "c9")
		if ok || !errors.Is(err, ErrStorage) {
			t.Errorf("%s: getGateReason = ok %v, err %v; want an ErrStorage refusal", name, ok, err)
		}
		j = memJournal() // a fresh journal: put is insert-once
	}
}

// Two drivers racing to write one call's reason record get one winner, and both read the winner's
// bytes (A1).
func TestPutGateReason_InsertOnceWinner(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	var wg sync.WaitGroup
	wins := make([]gateReasonRecord, 8)
	inserted := make([]bool, 8)
	for i := range wins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := gateReasonRecord{Format: gateReasonFormat, ToolUseID: "c1", Reasons: []gateReason{capReason("rule" + string(rune('a'+i)))}}
			got, ins, err := putGateReason(ctx, j, "r", rec)
			if err != nil {
				t.Error(err)
			}
			wins[i], inserted[i] = got, ins
		}()
	}
	wg.Wait()
	n := 0
	for i := range wins {
		if inserted[i] {
			n++
		}
		if !slices.Equal(wins[i].Reasons, wins[0].Reasons) {
			t.Fatalf("drivers read different records: %+v vs %+v", wins[i], wins[0])
		}
	}
	if n != 1 {
		t.Fatalf("%d drivers inserted the record, want 1", n)
	}
}

// key is injective over the fields' boundaries: moving a character across any two adjacent fields
// gives another key, so union never drops a reason for looking like another.
func TestGateReasonKey_BoundariesDoNotCollide(t *testing.T) {
	pairs := [][2]gateReason{
		{{Kind: "a", Source: "bc"}, {Kind: "ab", Source: "c"}},
		{{Kind: "k", Source: "a", Ref: "bc"}, {Kind: "k", Source: "ab", Ref: "c"}},
		{{Kind: "k", Ref: "a", Rule: "bc"}, {Kind: "k", Ref: "ab", Rule: "c"}},
		{{Kind: "k", Ref: "a1", Pos: 2}, {Kind: "k", Ref: "a", Pos: 12}},
		{{Kind: "k", Pos: 1, Rule: "2x"}, {Kind: "k", Pos: 12, Rule: "x"}},
		{{Kind: "k", Source: "a:b"}, {Kind: "k", Source: "a", Ref: "b"}},
		{{Kind: "k", Source: "1:a"}, {Kind: "k", Ref: "1:a"}},
	}
	for _, p := range pairs {
		if p[0].key() == p[1].key() {
			t.Errorf("%+v and %+v share the key %q", p[0], p[1], p[0].key())
		}
		if got := unionReasons([]gateReason{p[0]}, []gateReason{p[1]}); len(got) != 2 {
			t.Errorf("union of %+v and %+v kept %d reasons, want 2", p[0], p[1], len(got))
		}
	}
}
