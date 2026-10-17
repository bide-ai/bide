package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// askTool interrupts on its first run to collect a typed value, then returns it.
type askTool struct {
	name   string
	safety Safety
	key    string
	calls  *int
	got    *string
}

// Spec describes the tool to the agent (see Tool).
func (t *askTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: t.safety}
}

func (t *askTool) Call(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	*t.calls++
	v, err := Interrupt[string](ctx, t.key, "what should I use?")
	if err != nil {
		return nil, err
	}
	if t.got != nil {
		*t.got = v
	}
	return json.Marshal(map[string]string{"used": v})
}

// A tool pauses via Interrupt; Resume supplies a typed value; re-running continues.
func TestInterrupt_PausesAndResumesTyped(t *testing.T) {
	store := memJournal()
	var calls int
	var got string
	tool := &askTool{name: "ask", safety: Safety{ReadOnly: true}, key: "q", calls: &calls, got: &got}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "ask", `{}`), textTurn("done")}}
	a := mustNew(m, store, WithTools(tool))

	_, err := a.Run(context.Background(), "r", UserText("hi"))
	var intr *InterruptPending
	if !errors.As(err, &intr) {
		t.Fatalf("err = %v, want *InterruptPending", err)
	}
	if intr.Name != "q" || intr.Prompt != "what should I use?" {
		t.Fatalf("interrupt = %+v", intr)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times before resume, want 1", calls)
	}

	if err := store.AnswerInterrupt(context.Background(), "r", "q", "hello-human"); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	res, err := a.Run(context.Background(), "r", UserText("hi")) // same agent, resumes
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	out := res.Message
	if textOf(out) != "done" {
		t.Fatalf("answer = %q", textOf(out))
	}
	if got != "hello-human" {
		t.Fatalf("tool received %q, want hello-human", got)
	}
	if calls != 2 {
		t.Fatalf("tool ran %d times total, want 2 (re-run to resolve)", calls)
	}
}

// A struct resume value round-trips through the journal.
func TestInterrupt_StructValue(t *testing.T) {
	store := memJournal()
	type choice struct {
		Option string `json:"option"`
		Weight int    `json:"weight"`
	}
	var picked choice
	tool := MustFunc("pick", "", func(ctx context.Context, _ struct{}) (string, error) {
		c, err := Interrupt[choice](ctx, "pick", nil)
		if err != nil {
			return "", err
		}
		picked = c
		return c.Option, nil
	}, WithSafety(Safety{ReadOnly: true}))
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "pick", `{}`), textTurn("ok")}}
	a := mustNew(m, store, WithTools(tool))

	if _, err := a.Run(context.Background(), "r", UserText("hi")); !errorsAsInterrupted(err) {
		t.Fatalf("want interrupt, got %v", err)
	}
	if err := store.AnswerInterrupt(context.Background(), "r", "pick", choice{Option: "b", Weight: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r", UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if picked.Option != "b" || picked.Weight != 3 {
		t.Fatalf("picked = %+v, want {b 3}", picked)
	}
}

// Interrupt from a non-retry-safe tool is a misuse and fails with ErrConfig (rather than
// silently halting on resume).
func TestInterrupt_RequiresRetrySafe(t *testing.T) {
	store := memJournal()
	var calls int
	tool := &askTool{name: "write", safety: Safety{}, key: "q", calls: &calls} // not retry-safe
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "write", `{}`), textTurn("done")}}
	a := mustNew(m, store, WithTools(tool))

	_, err := a.Run(context.Background(), "r", UserText("hi"))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want errors.Is ErrConfig", err)
	}
	if errorsAsInterrupted(err) {
		t.Fatal("a non-retry-safe interrupt must not surface as a resumable pause")
	}
}

// Interrupt outside a running agent (no run context) is a config error.
func TestInterrupt_OutsideRun(t *testing.T) {
	_, err := Interrupt[string](context.Background(), "k", nil)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig", err)
	}
}

func errorsAsInterrupted(err error) bool {
	var i *InterruptPending
	return errors.As(err, &i)
}

type randVerifier struct{ id string }

func (randVerifier) Alg() Alg            { return fakeAlg }
func (v randVerifier) PublicKey() []byte { return []byte(v.id) }
func (v randVerifier) KeyIDs() []string  { return []string{"rand:" + v.id} }
func (v randVerifier) Verify(m, sig []byte) bool {
	return len(sig) > 1 && bytes.Equal(sig[1:], fakeSign(v.id, m))
}

// A randomized scheme (ECDSA, as KMS/HSM keys sign) gives one decision different signature bytes.
// Concurrent submissions of that one decision (a client retry racing its first attempt) must
// still report exactly one write, and record one decision.
func TestRev282_RandomizedConcurrentReportWritten(t *testing.T) {
	ctx := context.Background()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := func(id string) (ApproverVerifier, bool) { return randVerifier{id}, id == "a" }
	store := NewMemStore()
	j := mustJournal(store)
	var charged int
	if _, err := mofnRun(j, "r", true, pol, vf, &charged); err == nil {
		t.Fatal("run did not pause")
	}
	base := fakeSign("a", ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), "a", true, testDecidedAt))
	const n = 8
	wrote := make([]bool, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg,
				Signature: append([]byte{byte(i)}, base...)}
			<-start
			if err := SubmitDecision(ctx, mustJournal(store), d, WithDecisionCheck(vf), ReportWritten(&wrote[i])); err != nil {
				t.Logf("call %d: %v", i, err)
			}
		})
	}
	close(start)
	wg.Wait()
	got := 0
	for _, w := range wrote {
		if w {
			got++
		}
	}
	recs, _ := j.History(ctx, "r")
	nrec := 0
	for _, r := range recs {
		if r.Kind == StepApproval && r.Approver() == "a" {
			nrec++
		}
	}
	t.Logf("wrote=%v records=%d", wrote, nrec)
	if got != 1 || nrec != 1 {
		t.Fatalf("one decision, re-signed by a randomized scheme: %d submissions report a write and %d records are journaled; want 1 and 1", got, nrec)
	}
}

func pausedOne(t *testing.T) (*Journal, ApproverVerifierFor, []byte) {
	t.Helper()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := func(id string) (ApproverVerifier, bool) { return randVerifier{id}, id == "a" }
	j := mustJournal(NewMemStore())
	var charged int
	if _, err := mofnRun(j, "r", true, pol, vf, &charged); err == nil {
		t.Fatal("run did not pause")
	}
	return j, vf, fakeSign("a", ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), "a", true, testDecidedAt))
}

func decisionRecords(t *testing.T, j *Journal) []Record {
	t.Helper()
	recs, _ := j.History(context.Background(), "r")
	var out []Record
	for _, r := range recs {
		if r.Kind == StepApproval && r.Approver() == "a" {
			out = append(out, r)
		}
	}
	return out
}

// The step name is the decision's identity, never its signature bytes: two re-signings of one
// decision share it, and any other approver, verdict, scheme, time or call has another.
func TestDecisionStepIsTheDecisionsIdentity(t *testing.T) {
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte("one")}
	e := d
	e.Signature = []byte("two, a randomized re-signing")
	if DecisionStep(d) != DecisionStep(e) {
		t.Fatal("a re-signing of one decision has another name")
	}
	for name, f := range map[string]func(*Decision){
		"approver": func(x *Decision) { x.ApproverID = "b" }, "verdict": func(x *Decision) { x.Approved = false },
		"alg": func(x *Decision) { x.Alg = "other" }, "time": func(x *Decision) { x.DecidedAt = x.DecidedAt.Add(time.Second) },
		"call": func(x *Decision) { x.ToolUseID = "c2" },
	} {
		x := d
		f(&x)
		if DecisionStep(x) == DecisionStep(d) {
			t.Errorf("%s: same name for a different decision", name)
		}
	}
}

// A decision journaled under the previous, signature-derived name still counts, and resubmitting it
// (even re-signed) adds nothing.
func TestLegacyNamedDecisionStillCountsAndResubmitsAsNoop(t *testing.T) {
	ctx := context.Background()
	j, vf, base := pausedOne(t)
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: append([]byte{1}, base...)}
	legacy := approvalDecisionStep("c1", "a", true, fakeAlg, FormatTimestamp(testDecidedAt), d.Signature)
	if _, err := j.do(ctx, "r", legacy, func(context.Context) (Record, error) {
		return Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
			Approver: "a", ApproverAlg: fakeAlg, Signature: d.Signature, DecidedAt: FormatTimestamp(testDecidedAt), RecordedAt: FormatTimestamp(testDecidedAt)}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var charged int
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	if _, err := mofnRun(j, "r", false, pol, vf, &charged); err != nil || charged != 1 {
		t.Fatalf("the legacy-named decision does not count: %v charged %d", err, charged)
	}
	d2 := d
	d2.Signature = append([]byte{2}, base...)
	var wrote bool
	if err := SubmitDecision(ctx, j, d2, WithDecisionCheck(vf), ReportWritten(&wrote)); err != nil || wrote {
		t.Fatalf("resubmitting a legacy-named decision: err %v wrote %v", err, wrote)
	}
	if n := len(decisionRecords(t, j)); n != 1 {
		t.Fatalf("%d records", n)
	}
}

// A record under the decision's identity name that is not the approver's valid decision (a junk
// signature written without a check) must not take the name from the real decision: the real one is
// recorded under the signature-derived name, and counts.
func TestJunkAtTheIdentityNameDoesNotLockOutTheRealDecision(t *testing.T) {
	ctx := context.Background()
	j, vf, base := pausedOne(t)
	junk := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte{9, 9, 9}}
	if err := SubmitDecision(ctx, j, junk); err != nil {
		t.Fatal(err)
	}
	real := junk
	real.Signature = append([]byte{1}, base...)
	var wrote bool
	if err := SubmitDecision(ctx, j, real, WithDecisionCheck(vf), ReportWritten(&wrote)); err != nil || !wrote {
		t.Fatalf("real decision: err %v wrote %v", err, wrote)
	}
	var charged int
	if _, err := mofnRun(j, "r", false, &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}, vf, &charged); err != nil || charged != 1 {
		t.Fatalf("the real decision does not count behind the junk: %v charged %d", err, charged)
	}
}

// F1: a decision journaled by a release that names by signature, resubmitted without
// WithDecisionCheck (a retry after a lost response, or a rolling deploy where an old and a new
// binary both take submissions), used to be a no-op (same signature, same name). It now writes a
// second record of the same decision and ReportWritten says true, so a service acting once per
// recorded decision acts twice.
func TestUncheckedResubmitOfLegacyNamedDecision(t *testing.T) {
	ctx := context.Background()
	j, _, base := pausedOne(t)
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: append([]byte{1}, base...)}
	legacy := approvalDecisionStep("c1", "a", true, fakeAlg, FormatTimestamp(testDecidedAt), d.Signature)
	if _, err := j.do(ctx, "r", legacy, func(context.Context) (Record, error) {
		return Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
			Approver: "a", ApproverAlg: fakeAlg, Signature: d.Signature, DecidedAt: FormatTimestamp(testDecidedAt), RecordedAt: FormatTimestamp(testDecidedAt)}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var wrote bool
	if err := SubmitDecision(ctx, j, d, ReportWritten(&wrote)); err != nil {
		t.Fatal(err)
	}
	if wrote || len(decisionRecords(t, j)) != 1 {
		t.Fatalf("identical resubmission of a legacy-named decision: wrote=%v, %d records; want false and 1", wrote, len(decisionRecords(t, j)))
	}
}

// DecisionStep is the name SubmitDecision gives a decision first; it is not a lookup key. With a bad
// record at that name the real decision lives under another name, and HeldDecisionStep finds it.
func TestHeldDecisionStepFindsTheRecordWhereverItIs(t *testing.T) {
	ctx := context.Background()
	j, vf, base := pausedOne(t)
	junk := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte{9, 9, 9}}
	real := junk
	real.Signature = append([]byte{1}, base...)
	if _, ok, _, err := HeldDecisionStep(ctx, j, real, nil); err != nil || ok {
		t.Fatalf("before anything: %v %v", ok, err)
	}
	if err := SubmitDecision(ctx, j, junk); err != nil {
		t.Fatal(err)
	}
	if name, ok, _, err := HeldDecisionStep(ctx, j, real, nil); err != nil || ok {
		t.Fatalf("a junk record at the identity name is not the real decision: %q %v %v", name, ok, err)
	}
	if name, ok, ver, err := HeldDecisionStep(ctx, j, junk, vf); err != nil || !ok || ver || name != DecisionStep(junk) {
		t.Fatalf("junk: %q %v %v", name, ok, err)
	}
	if err := SubmitDecision(ctx, j, real, WithDecisionCheck(vf)); err != nil {
		t.Fatal(err)
	}
	name, ok, ver, err := HeldDecisionStep(ctx, j, real, vf)
	if err != nil || !ok || !ver || name == DecisionStep(real) {
		t.Fatalf("fallback: %q %v %v", name, ok, err)
	}
	if r, _, _ := j.Get(ctx, "r", name); string(r.Signature()) != string(real.Signature) {
		t.Fatalf("the named record is not the real decision: %v", r.Signature())
	}
	// A decision under the legacy name is found too.
	l := real
	l.DecidedAt = testDecidedAt.Add(1e9)
	legacy := approvalDecisionStep("c1", "a", true, fakeAlg, FormatTimestamp(l.DecidedAt), l.Signature)
	if _, err := j.do(ctx, "r", legacy, func(context.Context) (Record, error) {
		return Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
			Approver: "a", ApproverAlg: fakeAlg, Signature: l.Signature, DecidedAt: FormatTimestamp(l.DecidedAt)}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if name, ok, _, err := HeldDecisionStep(ctx, j, l, nil); err != nil || !ok || name != legacy {
		t.Fatalf("legacy: %q %v %v", name, ok, err)
	}
}

// held is not valid: an invalid signature is held, and verified only when a verifier says so.
func TestHeldDecisionStepIsNotValidity(t *testing.T) {
	ctx := context.Background()
	j, vf, _ := pausedOne(t)
	junk := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte{9, 9, 9}}
	if err := SubmitDecision(ctx, j, junk); err != nil {
		t.Fatal(err)
	}
	if _, held, ver, err := HeldDecisionStep(ctx, j, junk, nil); err != nil || !held || ver {
		t.Fatalf("no verifier: held %v verified %v %v", held, ver, err)
	}
	if _, held, ver, err := HeldDecisionStep(ctx, j, junk, vf); err != nil || !held || ver {
		t.Fatalf("a junk signature: held %v verified %v %v", held, ver, err)
	}
}

// A resolver that does not know the approver, or hands back a nil verifier, verifies nothing and
// must not panic.
func TestHeldDecisionStepWithAnUnusableResolver(t *testing.T) {
	ctx := context.Background()
	j, _, base := pausedOne(t)
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: append([]byte{1}, base...)}
	if err := SubmitDecision(ctx, j, d); err != nil {
		t.Fatal(err)
	}
	for name, vf := range map[string]ApproverVerifierFor{
		"unknown": func(string) (ApproverVerifier, bool) { return nil, false },
		"nil":     func(string) (ApproverVerifier, bool) { var v *randVerifier; return v, true },
	} {
		if _, held, ver, err := HeldDecisionStep(ctx, j, d, vf); err != nil || !held || ver {
			t.Errorf("%s: held %v verified %v %v", name, held, ver, err)
		}
	}
}

// IsDecisionStepName is the one shape a decision's record name can have: the call's approval prefix,
// the approver, and 64 lowercase hex, whether it is the identity name, the fallback or a legacy name.
func TestIsDecisionStepName(t *testing.T) {
	d := Decision{RunID: "r", ToolUseID: "c1:x", ApproverID: "ops:1", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte("s")}
	at := FormatTimestamp(d.DecidedAt)
	for name, n := range map[string]string{
		"identity": DecisionStep(d), "fallback/legacy": approvalDecisionStep(d.ToolUseID, d.ApproverID, d.Approved, d.Alg, at, d.Signature),
	} {
		if !IsDecisionStepName(n, d.ToolUseID, d.ApproverID) {
			t.Errorf("%s name %q refused", name, n)
		}
	}
	good := DecisionStep(d)
	hexTail := good[len(good)-64:]
	prefix := good[:len(good)-64]
	for name, n := range map[string]string{
		"empty": "", "bogus": "approval:bogus\x1b[31m", "no hex": prefix, "short hex": prefix + hexTail[:63], "long hex": prefix + hexTail + "0",
		"upper hex": prefix + strings.ToUpper(hexTail), "non hex": prefix + strings.Repeat("g", 64), "control": prefix + hexTail[:63] + "\x1b",
		"other approver": approvalStep(d.ToolUseID) + ":other:" + hexTail, "other call": approvalStep("c2") + ":" + d.ApproverID + ":" + hexTail,
		"suffix": good + ":extra", "newline": good + "\n",
	} {
		if IsDecisionStepName(n, d.ToolUseID, d.ApproverID) {
			t.Errorf("%s: %q accepted", name, n)
		}
	}
}

// Decision times of bide.approval.v3. TestReview_* pin the behaviour found in review;
// FuzzReview_* are the fuzz targets that review ran.

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

	// A re-approval that ended in a denial is final, even after a redeploy drops the gate and
	// before the denial's result is journaled (a crash in between): the recorded outcome is the
	// re-approval tally, not the pre-v3 tally that passed.
	store3 := memJournal()
	charged = 0
	if _, err := mofnRun(store3, "r3", true, pol, vf, &charged); !isPause(err) {
		t.Fatalf("first drive = %v", err)
	}
	writeRaw(t, store3, "r3", ApprovalTallyStep("c1"), Record{Kind: StepValue, Result: old})
	deniedAgain, _ := json.Marshal(ApprovalTally{Need: 2, Approvers: pol.Approvers, Denied: 1, DeniedBy: []string{"ops"}, DecisionFormat: DecisionFormat, Supersedes: ApprovalTallyStep("c1")})
	writeRaw(t, store3, "r3", ApprovalRetallyStep("c1"), Record{Kind: StepValue, Result: deniedAgain})
	if _, err := mofnRun(store3, "r3", false, nil, vf, &charged); err != nil || charged != 0 {
		t.Fatalf("drive with the gate removed = %v, charged %d; want the recorded re-approval denial final", err, charged)
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

// TestRecordedTally_RetallyOnlyOverPreV3: the re-approval tally is the gate's outcome only over a
// tally that passed on pre-v3 decisions, the one case a gate writes it; beside a tally of this
// format (an injected record) it is ignored, as the gate ignores it.
func TestRecordedTally_RetallyOnlyOverPreV3(t *testing.T) {
	enc := func(tl ApprovalTally) Record { b, _ := json.Marshal(tl); return Record{Kind: StepValue, Result: b} }
	pre := enc(ApprovalTally{Need: 1, Approvers: []string{"ops"}, Approved: 1, ApprovedBy: []string{"ops"}})
	v3denied := enc(ApprovalTally{Need: 1, Approvers: []string{"ops"}, Denied: 1, DeniedBy: []string{"ops"}, DecisionFormat: DecisionFormat})
	re := enc(ApprovalTally{Need: 1, Approvers: []string{"ops"}, Approved: 1, ApprovedBy: []string{"ops"}, DecisionFormat: DecisionFormat, Supersedes: ApprovalTallyStep("c1")})
	re.Name = "re"
	got, ok := RecordedTally(map[string]Record{ApprovalTallyStep("c1"): pre, ApprovalRetallyStep("c1"): re}, "c1")
	if !ok || got.Name != "re" {
		t.Fatalf("over a pre-v3 tally: got %+v, want the re-approval tally", got)
	}
	got, ok = RecordedTally(map[string]Record{ApprovalTallyStep("c1"): v3denied, ApprovalRetallyStep("c1"): re}, "c1")
	if !ok || got.Name == "re" {
		t.Fatalf("beside a v3 denial: got the injected re-approval tally")
	}
	if _, ok := RecordedTally(map[string]Record{ApprovalRetallyStep("c1"): re}, "c1"); ok {
		t.Fatal("a re-approval tally with no tally counts as the outcome")
	}
}

// TestSubmitDecision_ApproverIDPrintable: an approver id outside printable ASCII (a line break, a
// space, an escape, a non-ASCII letter) is refused before anything is recorded: no policy can name
// it, and it would otherwise reach every report that lists the decisions a gate read.
func TestSubmitDecision_ApproverIDPrintable(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	for _, id := range []string{"x\nPASS: forged", "ops ", "\x1b[32mops", "оps", "ops\t"} {
		d := Decision{RunID: "r1", ToolUseID: "c1", ApproverID: id, Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: []byte("s")}
		if err := SubmitDecision(ctx, store, d); !errors.Is(err, ErrConfig) {
			t.Fatalf("SubmitDecision(approver %q) = %v, want ErrConfig", id, err)
		}
	}
	if recs, _ := store.History(ctx, "r1"); len(recs) != 0 {
		t.Fatalf("refused decisions were recorded: %d records", len(recs))
	}
}
