package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"
)

// nonCanonicalTimes are decision times that name a real instant (or try to) but not in the one
// form a decision time is signed in.
var nonCanonicalTimes = []string{
	"2026-10-13T12:14:03.123+00:00", // an offset, even a zero one
	"2026-10-13T14:14:03.123+02:00", // another offset
	"2026-10-13T12:14:03.123-00:00",
	"2026-10-13T12:14:03.1Z",         // one fractional digit
	"2026-10-13T12:14:03.12Z",        // two
	"2026-10-13T12:14:03.1230Z",      // four
	"2026-10-13T12:14:03.123456789Z", // nine
	"2026-10-13T12:14:03Z",           // no fraction
	"2026-10-13T12:14:03.Z",          // an empty fraction
	"2026-10-13t12:14:03.123Z",       // lowercase t
	"2026-10-13T12:14:03.123z",       // lowercase z
	"2026-10-13t12:14:03.123z",       // both
	"2016-12-31T23:59:60.000Z",       // a leap second
	"2016-12-31T23:59:60.999Z",       // inside a leap second
	"2026-02-30T00:00:00.000Z",       // no such day
	"2026-10-13T24:00:00.000Z",       // no such hour
	"2026-13-01T00:00:00.000Z",       // no such month
	"2026-10-13 12:14:03.123Z",       // a space for T
	" 2026-10-13T12:14:03.123Z",      // padded
	"2026-10-13T12:14:03.123Z\n",     // a trailing newline
	"2026-10-13T12:14:03,123Z",       // a comma for the decimal point
	"+2026-10-13T12:14:03.123Z",      // a signed year
	"10000-01-01T00:00:00.000Z",      // a five-digit year
	"2026-1-13T12:14:03.123Z",        // an unpadded month
	"2026-10-13T12:14:03.123ZZ",      // trailing junk
	"2026-10-13T12:14:03.123Z00:00",  // an offset after the Z
	"２026-10-13T12:14:03.123Z",       // a fullwidth digit
	"",                               // nothing (an old decision: ReasonNoDecidedAt)
	"2026-10-13T12:14:03.123+0000",   // a colonless offset
	"2026-10-13T12:14:03.123 UTC",    // a zone name
	"Tue, 13 Oct 2026 12:14:03 GMT",  // another format altogether
	"1760357643123",                  // Unix millis
	"2026-10-13T12:14:03.123456Z",    // six fractional digits
	"2026-10-13T12:14:03.123Z ",      // a trailing space
	"2026-10-13T12:14:03.123​Z",      // a zero-width space
	"2026-10-13T12:14:03.123Z\x00",   // a NUL
	"2026-10-13T12:14:3.123Z",        // an unpadded second
	"2026-10-13T12:14:03.-12Z",       // a signed fraction
	"2026-10-13T12:14:03.12aZ",       // a letter in the fraction
	"2026-10-13T12:14:03.123+00",     // an hour-only offset
	"2026-10-13T12:14:03.123Z+00:00", // both
	"2026-10-13T12:14:03.123GMT",     // a zone abbreviation
	"0000-00-00T00:00:00.000Z",       // no such date
	"0000-01-01T00:00:00.000Z",       // year 0000, which other languages' date types cannot hold
	"0000-12-31T23:59:59.999Z",
	"2026-10-13T12:60:03.123Z",  // no such minute
	"2026-10-13T12:14:03.123−Z", // a minus sign
	"2026-10-13T12:14:03.123Ｚ",  // a fullwidth Z
	"2026-10-13 12:14:03.123Z",  // a narrow space
	"2026-10-13T12:14:03.123Ź", // Z with a combining accent
}

// TestParseTimestamp_Canonical: the one form is accepted and read back exactly; every other
// spelling is refused, including ones that name the same instant.
func TestParseTimestamp_Canonical(t *testing.T) {
	for _, s := range []string{"2026-10-13T12:14:03.123Z", "2026-10-13T12:14:03.000Z", "0001-01-01T00:00:00.000Z", "9999-12-31T23:59:59.999Z", "2024-02-29T23:59:59.999Z"} {
		got, err := ParseTimestamp(s)
		if err != nil {
			t.Fatalf("ParseTimestamp(%q) = %v, want it accepted", s, err)
		}
		if FormatTimestamp(got) != s || got.Location() != time.UTC {
			t.Fatalf("ParseTimestamp(%q) = %v, which formats as %q", s, got, FormatTimestamp(got))
		}
	}
	for _, s := range nonCanonicalTimes {
		if got, err := ParseTimestamp(s); err == nil {
			t.Fatalf("ParseTimestamp(%q) = %v, want it refused", s, got)
		}
	}
}

// TestFormatTimestamp: one instant formats one way, in UTC with a Z, to the millisecond
// (truncated), 24 bytes, whatever zone or precision it was given in.
func TestFormatTimestamp(t *testing.T) {
	east := time.FixedZone("east", 2*3600)
	cases := map[string]time.Time{
		"2026-10-13T12:14:03.123Z": time.Date(2026, 10, 13, 14, 14, 3, 123_999_999, east),
		"2026-10-13T12:14:03.000Z": time.Date(2026, 10, 13, 12, 14, 3, 0, time.UTC),
		"1999-12-31T23:59:59.999Z": time.Date(1999, 12, 31, 23, 59, 59, 999_999_999, time.UTC),
	}
	for want, at := range cases {
		if got := FormatTimestamp(at); got != want || len(got) != 24 {
			t.Fatalf("FormatTimestamp(%v) = %q, want %q", at, got, want)
		}
		if _, err := ParseTimestamp(FormatTimestamp(at)); err != nil {
			t.Fatalf("FormatTimestamp(%v) is not canonical: %v", at, err)
		}
	}
	// A time outside the years 0001 to 9999 has no canonical form.
	for _, at := range []time.Time{time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(0, 6, 1, 0, 0, 0, 0, time.UTC)} {
		if _, err := ParseTimestamp(FormatTimestamp(at)); err == nil {
			t.Fatalf("FormatTimestamp(%v) = %q, accepted as canonical", at, FormatTimestamp(at))
		}
	}
}

var timesSubject = ApprovalSubject{RunID: "r1", ToolUseID: "c1", ToolName: "refund", Args: []byte(`{"amount":120}`)}

// signedRecord is approver's decision on timesSubject at decidedAt, signed over the text
// signedAt (the same text, unless the test is forging one), and journaled as decided at decidedAt.
func signedRecord(name, approver string, approved bool, decidedAt, signedAt, recordedAt string) Record {
	sig := fakeSign(approver, decisionBytes(timesSubject, approver, approved, signedAt))
	return Record{Name: name, Kind: StepApproval, ToolUseID: "c1", Approved: approved, ApproverSignature: &ApproverSignature{
		Approver: approver, ApproverAlg: fakeAlg, Signature: sig, DecidedAt: decidedAt, RecordedAt: recordedAt}}
}

// TestApprovalDecisionBytes_Layout pins the signed encoding byte for byte, so any change to it is a
// deliberate one that this test names.
func TestApprovalDecisionBytes_Layout(t *testing.T) {
	at := time.Date(2026, 10, 13, 14, 14, 3, 123_456_789, time.FixedZone("east", 2*3600))
	got := ApprovalDecisionBytes(timesSubject, "ops", true, at)
	var want []byte
	want = append(want, "bide.approval.v3\n"...)
	for _, s := range []string{"r1", "c1", "refund"} {
		want = binary.BigEndian.AppendUint32(want, uint32(len(s)))
		want = append(want, s...)
	}
	args := sha256.Sum256([]byte(`{"amount":120}`))
	want = append(want, args[:]...)
	want = binary.BigEndian.AppendUint32(want, 3)
	want = append(want, "ops"...)
	want = append(want, 1)
	want = binary.BigEndian.AppendUint32(want, 24)
	want = append(want, "2026-10-13T12:14:03.123Z"...)
	if string(got) != string(want) {
		t.Fatalf("ApprovalDecisionBytes =\n%q\nwant\n%q", got, want)
	}
}

// TestTally_NonCanonicalDecidedAtNotCounted: a decision whose signed time is not in canonical form
// never counts, though its signature verifies over exactly the bytes recorded.
func TestTally_NonCanonicalDecidedAtNotCounted(t *testing.T) {
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	for _, s := range nonCanonicalTimes {
		rec := signedRecord("d1", "ops", true, s, s, "2026-10-13T12:14:04.000Z")
		got, checks := TallyApprovals([]Record{rec}, timesSubject, pol, fakeVerifiers("ops"))
		want := ReasonDecidedAt
		if s == "" {
			want = ReasonNoDecidedAt
		}
		if got.Passed() || len(checks) != 1 || checks[0].Counted || checks[0].Reason != want {
			t.Fatalf("decided_at %q: tally %+v, checks %+v; want not counted (%s)", s, got, checks, want)
		}
	}
	// The same decision in canonical form counts: the refusal is the form, not the signature.
	ok := signedRecord("d1", "ops", true, "2026-10-13T12:14:03.123Z", "2026-10-13T12:14:03.123Z", "")
	if got, _ := TallyApprovals([]Record{ok}, timesSubject, pol, fakeVerifiers("ops")); !got.Passed() {
		t.Fatalf("a canonical decision did not count: %+v", got)
	}
}

// TestTally_TamperedDecidedAt: the signature covers the decision time, so a decision whose time
// was changed after signing does not verify, by any amount and in either direction.
func TestTally_TamperedDecidedAt(t *testing.T) {
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	const signed = "2026-10-13T12:14:03.123Z"
	for _, changed := range []string{"2026-10-13T12:14:03.124Z", "2026-10-13T12:14:03.122Z", "2026-10-13T11:14:03.123Z", "2025-10-13T12:14:03.123Z", "2026-10-13T12:14:04.123Z"} {
		rec := signedRecord("d1", "ops", true, changed, signed, "")
		got, checks := TallyApprovals([]Record{rec}, timesSubject, pol, fakeVerifiers("ops"))
		if got.Passed() || checks[0].Counted || checks[0].Reason != ReasonBadSig {
			t.Fatalf("decided_at changed from %s to %s: tally %+v, checks %+v; want %q", signed, changed, got, checks, ReasonBadSig)
		}
	}
	// recorded_at is not signed: changing it changes nothing.
	rec := signedRecord("d1", "ops", true, signed, signed, "1999-01-01T00:00:00.000Z")
	if got, _ := TallyApprovals([]Record{rec}, timesSubject, pol, fakeVerifiers("ops")); !got.Passed() {
		t.Fatalf("recorded_at changed the count: %+v", got)
	}
}

// v2DecisionBytes is the encoding the previous version signed: no decision time.
func v2DecisionBytes(s ApprovalSubject, approverID string, approved bool) []byte {
	args := sha256.Sum256(canonicalArgs(s.Args))
	b := []byte("bide.approval.v2\n")
	for _, str := range []string{s.RunID, s.ToolUseID, s.ToolName} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(str)))
		b = append(b, str...)
	}
	b = append(b, args[:]...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(approverID)))
	b = append(b, approverID...)
	if approved {
		return append(b, 1)
	}
	return append(b, 0)
}

// TestTally_OldFormatDecisionRejected: a decision signed under the previous encoding does not
// count: as journaled (no decision time) it is refused with a reason that names the format, and
// with a time added after the fact its signature does not verify. SubmitDecision refuses one with
// no time before it is recorded.
func TestTally_OldFormatDecisionRejected(t *testing.T) {
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	sig := fakeSign("ops", v2DecisionBytes(timesSubject, "ops", true))
	old := Record{Name: "d1", Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{Approver: "ops", ApproverAlg: fakeAlg, Signature: sig}}
	got, checks := TallyApprovals([]Record{old}, timesSubject, pol, fakeVerifiers("ops"))
	if got.Passed() || checks[0].Reason != ReasonNoDecidedAt {
		t.Fatalf("old decision: tally %+v, checks %+v; want %q", got, checks, ReasonNoDecidedAt)
	}
	dressed := old
	dressed.ApproverSignature = &ApproverSignature{Approver: "ops", ApproverAlg: fakeAlg, Signature: sig, DecidedAt: "2026-10-13T12:14:03.123Z"}
	got, checks = TallyApprovals([]Record{dressed}, timesSubject, pol, fakeVerifiers("ops"))
	if got.Passed() || checks[0].Reason != ReasonBadSig {
		t.Fatalf("old decision with a time added: tally %+v, checks %+v; want %q", got, checks, ReasonBadSig)
	}
	err := SubmitDecision(context.Background(), memJournal(), Decision{RunID: "r1", ToolUseID: "c1", ApproverID: "ops", Approved: true, Alg: fakeAlg, Signature: sig})
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("SubmitDecision with no DecidedAt = %v, want ErrConfig", err)
	}
}

// TestTally_IgnoresTimes: the counting rule's result does not depend on either time. The same
// journal of decisions (approvals, denials, superseded and invalid ones, in a random order) is
// counted with every decision's times drawn at random, decision times running against journal
// order, and recorded times decades apart; the tally and every record's classification are those
// of the same journal with one fixed time.
func TestTally_IgnoresTimes(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	pol := ApprovalPolicy{Need: 3, Approvers: []string{"a", "b", "c", "d", "e"}}
	vf := fakeVerifiers("a", "b", "c", "d") // e has no key
	const fixed = "2026-10-13T12:14:03.123Z"
	randomTime := func() string {
		return FormatTimestamp(time.UnixMilli(rng.Int64N(253402300799999)).UTC()) // years 1970 to 9999
	}
	type dec struct {
		who      string
		approved bool
		forged   bool
	}
	for round := range 500 {
		var ds []dec
		for range 1 + rng.IntN(12) {
			ds = append(ds, dec{who: string(rune('a' + rng.IntN(6))), approved: rng.IntN(3) != 0, forged: rng.IntN(5) == 0})
		}
		build := func(timeOf func(i int) (string, string)) []Record {
			recs := make([]Record, len(ds))
			for i, d := range ds {
				decided, recorded := timeOf(i)
				recs[i] = signedRecord("d"+string(rune('0'+i)), d.who, d.approved, decided, decided, recorded)
				if d.forged {
					recs[i].ApproverSignature.Signature = fakeSign("mallory", decisionBytes(timesSubject, d.who, d.approved, decided))
				}
			}
			return recs
		}
		wantT, wantC := TallyApprovals(build(func(int) (string, string) { return fixed, fixed }), timesSubject, pol, vf)
		for _, timeOf := range []func(int) (string, string){
			func(int) (string, string) { return randomTime(), randomTime() },
			func(i int) (string, string) { // decision times running backwards through the journal
				return FormatTimestamp(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Hour)), ""
			},
			func(i int) (string, string) { // decided long after being recorded
				return "9999-12-31T23:59:59.999Z", "1970-01-01T00:00:00.000Z"
			},
		} {
			gotT, gotC := TallyApprovals(build(timeOf), timesSubject, pol, vf)
			if !reflect.DeepEqual(gotT, wantT) || !reflect.DeepEqual(gotC, wantC) {
				t.Fatalf("round %d, decisions %+v: with other times\n tally %+v\nchecks %+v\nwant\n tally %+v\nchecks %+v", round, ds, gotT, gotC, wantT, wantC)
			}
		}
	}
}

// TestSubmitDecision_Times: SubmitDecision journals the signed decision time in canonical form and
// the time it recorded the decision from WithClock's clock (else the context's run clock); an
// identical resubmission keeps the first record and its time. A zero decision time, or one with no
// canonical form, is refused.
func TestSubmitDecision_Times(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	decided := time.Date(2026, 10, 13, 14, 14, 3, 123_999_000, time.FixedZone("east", 2*3600))
	clock := time.Date(2026, 10, 13, 12, 20, 0, 456_000_000, time.UTC)
	sig := fakeSign("ops", ApprovalDecisionBytes(timesSubject, "ops", true, decided))
	d := Decision{RunID: "r1", ToolUseID: "c1", ApproverID: "ops", Approved: true, DecidedAt: decided, Alg: fakeAlg, Signature: sig}
	if err := SubmitDecision(ctx, store, d, WithClock(func() time.Time { return clock })); err != nil {
		t.Fatal(err)
	}
	later := clock.Add(time.Hour)
	if err := SubmitDecision(ctx, store, d, WithClock(func() time.Time { return later })); err != nil {
		t.Fatal(err)
	}
	// The context's clock, when no WithClock is given.
	d2 := d
	d2.ApproverID = "fin"
	d2.Signature = fakeSign("fin", ApprovalDecisionBytes(timesSubject, "fin", true, decided))
	if err := SubmitDecision(contextWithClock(ctx, func() time.Time { return later }), store, d2); err != nil {
		t.Fatal(err)
	}
	recs, err := store.History(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	var got []Record
	for _, r := range recs {
		if IsApprovalDecision(r, "c1") {
			got = append(got, r)
		}
	}
	if len(got) != 2 {
		t.Fatalf("decision records = %d, want 2 (the resubmission is a no-op)", len(got))
	}
	if got[0].DecidedAt() != "2026-10-13T12:14:03.123Z" || got[0].RecordedAt() != "2026-10-13T12:20:00.456Z" {
		t.Fatalf("ops decision times = %q, %q; want the signed time in UTC to the millisecond, and WithClock's first time", got[0].DecidedAt(), got[0].RecordedAt())
	}
	if got[1].RecordedAt() != FormatTimestamp(later) {
		t.Fatalf("fin recorded_at = %q, want the context's clock %q", got[1].RecordedAt(), FormatTimestamp(later))
	}
	if tally, _ := TallyApprovals(recs, timesSubject, ApprovalPolicy{Need: 2, Approvers: []string{"ops", "fin"}}, fakeVerifiers("ops", "fin")); !tally.Passed() {
		t.Fatalf("the submitted decisions do not count: %+v", tally)
	}
	for _, at := range []time.Time{{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		bad := d
		bad.DecidedAt = at
		if err := SubmitDecision(ctx, store, bad); !errors.Is(err, ErrConfig) {
			t.Fatalf("SubmitDecision with DecidedAt %v = %v, want ErrConfig", at, err)
		}
	}
	if err := SubmitDecision(ctx, store, d, WithClock(nil)); !errors.Is(err, ErrConfig) {
		t.Fatalf("SubmitDecision with WithClock(nil) = %v, want ErrConfig", err)
	}
}

// TestSubmitDecision_TimeInName: a copy of an approver's valid signature journaled first under
// another decision time cannot take the name the real decision needs: the real one is still
// recorded and counts.
func TestSubmitDecision_TimeInName(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	decided := time.Date(2026, 10, 13, 12, 14, 3, 123_000_000, time.UTC)
	sig := fakeSign("ops", ApprovalDecisionBytes(timesSubject, "ops", true, decided))
	forged := Decision{RunID: "r1", ToolUseID: "c1", ApproverID: "ops", Approved: true, DecidedAt: decided.Add(time.Second), Alg: fakeAlg, Signature: sig}
	if err := SubmitDecision(ctx, store, forged); err != nil {
		t.Fatal(err)
	}
	real := forged
	real.DecidedAt = decided
	if err := SubmitDecision(ctx, store, real); err != nil {
		t.Fatal(err)
	}
	recs, err := store.History(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	tally, checks := TallyApprovals(recs, timesSubject, ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, fakeVerifiers("ops"))
	if !tally.Passed() || len(checks) != 2 || checks[0].Reason != ReasonBadSig || !checks[1].Counted {
		t.Fatalf("tally %+v, checks %+v; want the copy refused and the real decision counted", tally, checks)
	}
}

// TestApprovalDecisionBytes_Vector pins the exact signed bytes of one fixed decision, the worked
// example in docs/design/design-mofn-approval.md (computed there independently of Go). A change
// to the signed encoding fails it: change the vector, the example and the version tag together.
func TestApprovalDecisionBytes_Vector(t *testing.T) {
	s := ApprovalSubject{RunID: "refund-42", ToolUseID: "refund-1", ToolName: "refund", Args: []byte(`{"order": 42, "amount": 120}`)}
	got := hex.EncodeToString(ApprovalDecisionBytes(s, "finance", true, time.Date(2026, 10, 13, 12, 14, 3, 123_000_000, time.UTC)))
	const want = "626964652e617070726f76616c2e76330a" + // "bide.approval.v3\n"
		"00000009" + "726566756e642d3432" + // "refund-42"
		"00000008" + "726566756e642d31" + // "refund-1"
		"00000006" + "726566756e64" + // "refund"
		"984c3a3505d933239fe461f7082f36e3c3a6caf30dac23c34d6c4caf03c96af7" + // SHA-256(`{"amount":120,"order":42}`)
		"00000007" + "66696e616e6365" + // "finance"
		"01" + // approved
		"00000018" + "323032362d31302d31335431323a31343a30332e3132335a" // "2026-10-13T12:14:03.123Z"
	if got != want {
		t.Fatalf("ApprovalDecisionBytes =\n%s\nwant\n%s", got, want)
	}
}

// TestGate_PreV3PassedTallyReapproval: a gate that finds a terminal tally which passed but carries
// no decision format (journaled before bide.approval.v3, on decisions with no signed time) does
// not run the call on it: it pauses (*ApprovalPending, Reason PendingReapproval) with a fresh
// count. Approvers sign again under v3, the gate passes, records the new outcome under
// ApprovalRetallyStep, and runs the call once; a later drive reuses that record. A pre-v3 denial
// stays final.
func TestGate_PreV3PassedTallyReapproval(t *testing.T) {
	ctx := context.Background()
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"ops", "fin"}}
	vf := fakeVerifiers("ops", "fin")
	store := memJournal()
	charged := 0
	if _, err := mofnRun(store, "r1", true, pol, vf, &charged); !isPause(err) {
		t.Fatalf("first drive = %v, want a pause", err)
	}
	// The run as an earlier version left it: two decisions with no signed time, and a tally that
	// passed on them, journaled before the tool ran.
	var names []string
	for _, id := range pol.Approvers {
		name := approvalStep("c1") + ":" + id + ":v2"
		writeRaw(t, store, "r1", name, Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{Approver: id, ApproverAlg: fakeAlg, Signature: []byte("v2")}})
		names = append(names, name)
	}
	old, _ := json.Marshal(ApprovalTally{Need: 2, Approvers: pol.Approvers, Approved: 2, ApprovedBy: pol.Approvers, Records: names})
	writeRaw(t, store, "r1", ApprovalTallyStep("c1"), Record{Kind: StepValue, Result: old})

	_, err := mofnRun(store, "r1", false, pol, vf, &charged)
	var pend *ApprovalPending
	if !errors.As(err, &pend) || pend.Reason != PendingReapproval || pend.Quorum == nil || pend.Quorum.Approved != 0 || pend.Quorum.Supersedes != ApprovalTallyStep("c1") || charged != 0 {
		t.Fatalf("resume on a pre-v3 passed tally = %v (pend %+v, charged %d); want a re-approval pause and the call not run", err, pend, charged)
	}
	subj := subjectOf(t, store, "r1", "c1")
	for _, id := range pol.Approvers {
		if err := decideAs(ctx, store, "r1", "c1", id, true, fakeSign(id, ApprovalDecisionBytes(subj, id, true, testDecidedAt))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mofnRun(store, "r1", false, pol, vf, &charged); err != nil || charged != 1 {
		t.Fatalf("drive after v3 re-approval = %v, charged %d; want the call run once", err, charged)
	}
	r, ok, err := store.Get(ctx, "r1", ApprovalRetallyStep("c1"))
	if err != nil || !ok {
		t.Fatalf("no re-approval tally recorded: %v", err)
	}
	re, err := decodeTally("r1", r)
	if err != nil || !re.Passed() || re.DecisionFormat != DecisionFormat || re.Supersedes != ApprovalTallyStep("c1") {
		t.Fatalf("re-approval tally = %+v, %v", re, err)
	}
	if _, err := mofnRun(store, "r1", false, pol, vf, &charged); err != nil || charged != 1 {
		t.Fatalf("a later drive = %v, charged %d; want the recorded outcome reused", err, charged)
	}

	// A pre-v3 denial is final: the call is denied without asking again.
	store2 := memJournal()
	charged = 0
	if _, err := mofnRun(store2, "r2", true, pol, vf, &charged); !isPause(err) {
		t.Fatalf("first drive = %v", err)
	}
	denied, _ := json.Marshal(ApprovalTally{Need: 2, Approvers: pol.Approvers, Denied: 1, DeniedBy: []string{"ops"}})
	writeRaw(t, store2, "r2", ApprovalTallyStep("c1"), Record{Kind: StepValue, Result: denied})
	if _, err := mofnRun(store2, "r2", false, pol, vf, &charged); err != nil || charged != 0 {
		t.Fatalf("resume on a pre-v3 denial = %v, charged %d; want it denied, not re-asked", err, charged)
	}
}

func isPause(err error) bool {
	var p *ApprovalPending
	return errors.As(err, &p)
}
