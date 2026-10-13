package agent

// Adversarial review of #244 (bide.approval.v3, decision times). TestReview_* reproduce findings
// and fail on the reviewed branch; FuzzReview_* are the fuzz targets the review ran.

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestReview_SubmitDecisionJournalsNonCanonicalRecordedAt: decided_at must have a canonical form
// (years 0001 to 9999) or SubmitDecision refuses it, but recorded_at, from WithClock or the
// context's clock, is formatted and journaled unchecked. A clock outside the range writes a
// recorded_at that ParseTimestamp refuses, through the public API, so every reader that shows it
// (VerifyApprovals' Times, the CLI, the viewer) gets a value that is not in the one form.
func TestReview_SubmitDecisionJournalsNonCanonicalRecordedAt(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	d := Decision{RunID: "r1", ToolUseID: "c1", ApproverID: "ops", Approved: true, DecidedAt: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC),
		Alg: "ed25519", Signature: make([]byte, 64)}
	far := time.Date(12026, 1, 1, 0, 0, 0, 0, time.UTC)
	err := SubmitDecision(ctx, j, d, WithClock(func() time.Time { return far }))
	if err != nil {
		return // refused: the finding is fixed
	}
	recs, herr := j.History(ctx, "r1")
	if herr != nil {
		t.Fatal(herr)
	}
	for _, r := range recs {
		if IsApprovalDecision(r, "c1") {
			if _, perr := ParseTimestamp(r.RecordedAt()); perr != nil {
				t.Fatalf("SubmitDecision journaled recorded_at %q, which ParseTimestamp refuses: %v", r.RecordedAt(), perr)
			}
		}
	}
}

// FuzzReview_ParseTimestamp: whatever ParseTimestamp accepts is 24 bytes, ends in "Z", formats
// back to itself, and parses again to the same instant.
func FuzzReview_ParseTimestamp(f *testing.F) {
	for _, s := range append([]string{"2026-10-13T12:14:03.123Z", "0000-01-01T00:00:00.000Z", "9999-12-31T23:59:59.999Z"}, nonCanonicalTimes...) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseTimestamp(s)
		if err != nil {
			return
		}
		if len(s) != 24 || s[23] != 'Z' || FormatTimestamp(got) != s || got.Location() != time.UTC {
			t.Fatalf("ParseTimestamp accepted %q as %v (formats %q)", s, got, FormatTimestamp(got))
		}
		again, err := ParseTimestamp(FormatTimestamp(got))
		if err != nil || !again.Equal(got) {
			t.Fatalf("round trip of %q: %v, %v", s, again, err)
		}
	})
}

// FuzzReview_FormatTimestamp: for any instant, FormatTimestamp's output either is refused by
// ParseTimestamp or reads back as the instant truncated to the millisecond; and an instant in
// the years 0001 to 9999 is never refused.
func FuzzReview_FormatTimestamp(f *testing.F) {
	f.Add(int64(1760357643), int64(123456789))
	f.Add(int64(-62167219200), int64(500000)) // 0000-01-01
	f.Add(int64(253402300799), int64(999999999))
	f.Add(int64(-62135596800), int64(-1))
	f.Fuzz(func(t *testing.T, sec, nsec int64) {
		in := time.Unix(sec, nsec).UTC()
		s := FormatTimestamp(in)
		got, err := ParseTimestamp(s)
		inRange := in.Year() >= 1 && in.Year() <= 9999 // year 0000 is refused since the review
		if err != nil {
			if inRange {
				t.Fatalf("FormatTimestamp(%v) = %q, refused: %v", in, s, err)
			}
			return
		}
		want := time.UnixMilli(in.UnixMilli()).UTC()
		if in.Before(time.Unix(0, 0)) && in.Nanosecond()%int(time.Millisecond) != 0 {
			// UnixMilli rounds toward zero for negative times; Truncate since the zero time is
			// what FormatTimestamp uses, so only require got within the same millisecond.
			if d := in.Sub(got); d < 0 || d >= time.Millisecond {
				t.Fatalf("FormatTimestamp(%v) = %q reads back %v, %v away", in, s, got, d)
			}
			return
		}
		if !got.Equal(want) {
			t.Fatalf("FormatTimestamp(%v) = %q reads back %v, want %v", in, s, got, want)
		}
	})
}

// FuzzReview_DecisionRecord: a journaled decision decodes (DecodeRecord) and encodes back to a
// record with the same decision fields, and counting it never panics.
func FuzzReview_DecisionRecord(f *testing.F) {
	seed, _ := json.Marshal(Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
		Approver: "ops", ApproverAlg: "ed25519", Signature: make([]byte, 64), DecidedAt: "2026-10-13T12:14:03.123Z", RecordedAt: "2026-10-13T12:14:04.000Z"}})
	f.Add(seed)
	f.Add([]byte(`{"kind":"approval","tool_use_id":"c1","approved":true,"approver_signature":{"approver":"ops","decided_at":"2026-10-13T12:14:03.123Z"}}`))
	f.Add([]byte(`{"kind":"approval","approver_signature":{"decided_at":"","recorded_at":""}}`))
	s := ApprovalSubject{RunID: "r1", ToolUseID: "c1", ToolName: "charge", Args: json.RawMessage(`{"amount":1}`)}
	p := ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	vf := func(string) (ApproverVerifier, bool) { return fuzzVerifier{}, true }
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := DecodeRecord(b)
		if err != nil {
			return
		}
		r.Name = "approval:c1:ops:x"
		tally, _ := TallyApprovals([]Record{r}, s, p, vf)
		if tally.Approved+tally.Denied > 1 {
			t.Fatalf("one record counted %d times", tally.Approved+tally.Denied)
		}
		if tally.Approved+tally.Denied == 1 && !canonicalTimestamp(r.DecidedAt()) {
			t.Fatalf("counted a decision whose decided_at %q is not canonical", r.DecidedAt())
		}
		out, err := json.Marshal(r)
		if err != nil {
			return
		}
		r2, err := DecodeRecord(out)
		if err != nil {
			t.Fatalf("re-decode of %s: %v", out, err)
		}
		if r2.DecidedAt() != r.DecidedAt() || r2.RecordedAt() != r.RecordedAt() || r2.Approver() != r.Approver() || r2.Approved != r.Approved {
			t.Fatalf("decision fields changed across encode: %+v -> %+v", r.ApproverSignature, r2.ApproverSignature)
		}
	})
}

// fuzzVerifier accepts every signature, so the fuzzer reaches the counting branches.
type fuzzVerifier struct{}

func (fuzzVerifier) Alg() Alg                { return "ed25519" }
func (fuzzVerifier) Verify(_, _ []byte) bool { return true }
func (fuzzVerifier) KeyIDs() []string        { return []string{"ed25519:fuzz"} }
