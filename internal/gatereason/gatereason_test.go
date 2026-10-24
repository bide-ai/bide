package gatereason

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// recs adapts name/result pairs to Records.
func recs(pairs ...string) Records {
	return func(yield func(string, []byte) bool) {
		for i := 0; i+1 < len(pairs); i += 2 {
			if !yield(pairs[i], []byte(pairs[i+1])) {
				return
			}
		}
	}
}

const taintRecord = `{"format":"bide.gate-reason.v1","tool_use_id":"c3","reasons":[` +
	`{"kind":"taint","source":"inbound-email","ref":"tool:read_email/c1","pos":4},` +
	`{"kind":"cap","source":"","ref":"","rule":"refund_one","action":"gate","why":"over","currency":"USD","amount":600000,"limit":500000}],` +
	`"policy_digest":"d","gate":{"need":2,"approvers":["a","b"]},"recorded_at":"2026-10-24T10:43:00.000Z"}`

func TestFind_TaintOnTheInstanceCapsOnTheCall(t *testing.T) {
	r := recs("gate-reason:c3", taintRecord)
	taint := Find("c3#taint", r)
	if len(taint) != 1 || taint[0].Kind != KindTaint || taint[0].Source != "inbound-email" || taint[0].Ref != "tool:read_email/c1" || taint[0].Pos != 4 || taint[0].At != "2026-10-24T10:43:00.000Z" {
		t.Fatalf("instance reasons = %+v, want the taint reason alone", taint)
	}
	caps := Find("c3", r)
	if len(caps) != 1 || caps[0].Kind != KindCap || caps[0].Rule != "refund_one" || *caps[0].Amount != 600000 || caps[0].Limit != 500000 || caps[0].Action != "gate" {
		t.Fatalf("own gate reasons = %+v, want the cap reason alone", caps)
	}
}

func TestFind_NothingForAnotherCallOrAnUnreadableRecord(t *testing.T) {
	for name, r := range map[string]Records{
		"another call":      recs("gate-reason:c9", strings.Replace(taintRecord, `"c3"`, `"c9"`, 1)),
		"no record":         recs("approval-pending:c3", taintRecord),
		"not JSON":          recs("gate-reason:c3", "x"),
		"another format":    recs("gate-reason:c3", strings.Replace(taintRecord, "v1", "v9", 1)),
		"an unknown member": recs("gate-reason:c3", strings.Replace(taintRecord, `"policy_digest"`, `"future":1,"policy_digest"`, 1)),
		"a duplicate":       recs("gate-reason:c3", strings.Replace(taintRecord, `"tool_use_id":"c3"`, `"tool_use_id":"c3","tool_use_id":"c3"`, 1)),
		"a case variant":    recs("gate-reason:c3", strings.Replace(taintRecord, `"reasons"`, `"Reasons"`, 1)),
	} {
		if got := Find("c3#taint", r); len(got) != 0 {
			t.Errorf("%s: %+v, want none", name, got)
		}
	}
}

// The engine's record is the first that reads; a record that names another call is skipped.
func TestFind_TheFirstRecordThatReadsIsTheEngines(t *testing.T) {
	other := strings.Replace(taintRecord, "inbound-email", "planted", 1)
	got := Find("c3#taint", recs("gate-reason:x", "junk", "gate-reason:c3", taintRecord, "gate-reason:c3-b", other))
	if len(got) != 1 || got[0].Source != "inbound-email" {
		t.Fatalf("reasons = %+v, want the first record's", got)
	}
}

// Host-declared strings are shown as they can be shown safely.
func TestFind_StringsAreBoundedAndEscaped(t *testing.T) {
	long := strings.Repeat("é", 100)
	rec := map[string]any{"format": recordFormat, "tool_use_id": "c3", "recorded_at": "t", "reasons": []map[string]any{
		{"kind": "taint", "source": "evil‮\x1b[2J", "ref": long, "pos": 1},
		{"kind": "taint", "source": "ok", "ref": "input", "pos": 2},
	}}
	b, _ := json.Marshal(rec)
	got := Find("c3#taint", recs("gate-reason:c3", string(b)))
	if len(got) != 2 {
		t.Fatalf("reasons = %+v", got)
	}
	if strings.ContainsAny(got[0].Source, "‮\x1b") || !strings.HasPrefix(got[0].Source, `"`) {
		t.Errorf("source = %q, want the non-printing characters escaped", got[0].Source)
	}
	if len(got[0].Ref) > MaxText+2 {
		t.Errorf("ref is %d bytes, want it cut near %d", len(got[0].Ref), MaxText)
	}
	if got[1].Source != "ok" {
		t.Errorf("a plain name was changed: %q", got[1].Source)
	}
}

func TestFind_AtMostMaxReasons(t *testing.T) {
	var rs []map[string]any
	for i := 0; i < 200; i++ {
		rs = append(rs, map[string]any{"kind": "taint", "source": "s", "ref": "input", "pos": i})
	}
	b, _ := json.Marshal(map[string]any{"format": recordFormat, "tool_use_id": "c3", "reasons": rs})
	if got := Find("c3#taint", recs("gate-reason:c3", string(b))); len(got) != maxReasons {
		t.Fatalf("%d reasons, want %d", len(got), maxReasons)
	}
}

func TestReason_Line(t *testing.T) {
	amt := int64(600000)
	for _, tc := range []struct {
		r    Reason
		want string
	}{
		{Reason{Kind: KindTaint, Source: "inbound-email", Ref: "tool:read_email/c1", Pos: 4}, "untrusted content from inbound-email (read by tool read_email, call c1, journal position 4)"},
		{Reason{Kind: KindTaint, Source: "chat", Ref: "input", Pos: 1}, "untrusted content from chat (the run's input, journal position 1)"},
		{Reason{Kind: KindTaint, Source: "kb", Ref: "retrieval", Pos: 2}, "untrusted content from kb (retrieved documents, journal position 2)"},
		{Reason{Kind: KindCap, Rule: "refund_one", Action: "gate", Why: "over", Currency: "USD", Amount: &amt, Limit: 500000}, "cap refund_one: this call 6000.00 USD, limit 5000.00 USD (over, gate)"},
		{Reason{Kind: KindCap, Rule: "r", Action: "refuse", Why: "not_assessable", Currency: "USD", Limit: 100}, "cap r: an amount that could not be read, limit 1.00 USD (not_assessable, refuse)"},
	} {
		if got := tc.r.Line(); got != tc.want {
			t.Errorf("Line = %q, want %q", got, tc.want)
		}
	}
}

func TestMoney(t *testing.T) {
	for _, tc := range []struct {
		minor int64
		cur   string
		want  string
	}{
		{500000, "USD", "5000.00 USD"}, {5, "USD", "0.05 USD"}, {-5, "USD", "-0.05 USD"}, {12, "JPY", "12 JPY"}, {1234, "KWD", "1.234 KWD"},
		{math.MinInt64, "USD", "-92233720368547758.08 USD"}, {7, "ZZZ", `7 minor units of "ZZZ"`},
	} {
		if got := Money(tc.minor, tc.cur); got != tc.want {
			t.Errorf("Money(%d, %s) = %q, want %q", tc.minor, tc.cur, got, tc.want)
		}
	}
}
