package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"
)

// turnModel answers by the assistant turns already in the request, so a resumed drive gets the
// turn after the ones its journal replays.
type turnModel [][]Emit

func (m turnModel) Stream(_ context.Context, req Request) (*Stream, error) {
	i := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleAssistant {
			i++
		}
	}
	turn := m[min(i, len(m)-1)]
	ch := make(chan Emit, len(turn))
	for _, e := range turn {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

// lookupRefund is a model that looks an order up (read-only), refunds it, and answers.
var lookupRefund = turnModel{toolTurn("c1", "lookup", `{}`), toolTurn("c2", "refund", `{}`), textTurn("refunded")}

// refundTools are lookupRefund's tools: refund is a side effect, gated when gate is set.
func refundTools(lookups, refunds *int, gate *ApprovalPolicy) Option {
	return WithTools(
		&countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: lookups},
		&countingTool{name: "refund", approval: gate, calls: refunds})
}

// A first drive records when it started, by its clock: the run's clock over the agent's, else
// the wall clock, as UTC RFC 3339 to the millisecond.
func TestRunStart_StartedAt(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 13, 3, 4, 5, 678_901_234, time.FixedZone("x", 2*3600))
	agentAt := at.Add(-time.Hour)
	var n int
	j := memJournal()
	a := mustNew(lookupRefund, j, refundTools(&n, &n, nil), WithClock(func() time.Time { return agentAt }))
	if _, err := a.Run(ctx, "run-clock", UserText("go"), WithClock(func() time.Time { return at })); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "agent-clock", UserText("go")); err != nil {
		t.Fatal(err)
	}
	for runID, want := range map[string]string{"run-clock": "2026-10-13T01:04:05.678Z", "agent-clock": "2026-10-13T00:04:05.678Z"} {
		st, ok, err := RecordedStart(ctx, j, runID)
		if err != nil || !ok || st.StartedAt != want {
			t.Fatalf("%s: StartedAt %q (ok %v, err %v), want %q", runID, st.StartedAt, ok, err, want)
		}
		got, ok := st.Started()
		if wantT, _ := time.Parse(time.RFC3339, want); !ok || !got.Equal(wantT) {
			t.Fatalf("%s: Started = %v, %v", runID, got, ok)
		}
	}

	before := time.Now().Truncate(time.Millisecond)
	if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil)).Run(ctx, "wall", UserText("go")); err != nil {
		t.Fatal(err)
	}
	st, _, _ := RecordedStart(ctx, j, "wall")
	got, ok := st.Started()
	if !ok || got.Before(before) || got.After(time.Now()) || got.Location() != time.UTC || !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(st.StartedAt) {
		t.Fatalf("wall clock: StartedAt %q (%v, %v), want UTC RFC 3339 ms within the run", st.StartedAt, got, ok)
	}

	// A later drive does not rewrite it.
	if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil)).Run(ctx, "run-clock", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := RecordedStart(ctx, j, "run-clock"); st.StartedAt != "2026-10-13T01:04:05.678Z" {
		t.Fatalf("after a later drive: StartedAt %q", st.StartedAt)
	}

	// Started reads only an RFC 3339 time.
	for v, want := range map[string]bool{"": false, "yesterday": false, "2026-10-13": false, "2026-10-13T01:02:03Z": true, "2026-10-13T01:02:03.456+02:00": true} {
		if _, ok := (RunStart{StartedAt: v}).Started(); ok != want {
			t.Errorf("Started(%q) ok = %v, want %v", v, ok, want)
		}
	}

	// The commonest start, with its time, is read without the JSON decoder.
	e, _, err := j.store.Get(ctx, "wall", runStartStep)
	if err != nil {
		t.Fatal(err)
	}
	if fast, ok := decodePlainStart(e.Data); !ok || fast.StartedAt != st.StartedAt {
		t.Fatalf("decodePlainStart(%s) = %+v, %v", e.Data, fast, ok)
	}
}

// startTimeRE matches run:start's start time member, as the journal writes it.
var startTimeRE = regexp.MustCompile(`,"started_at":"[^"]*"`)

// startVariants are run:start's start time edits a resume must not notice: none (the record as a
// version that did not record one wrote it), another time, and a value that is not a time.
var startVariants = map[string]func([]byte) []byte{
	"as written": func(b []byte) []byte { return b },
	"none":       func(b []byte) []byte { return startTimeRE.ReplaceAll(b, nil) },
	"another": func(b []byte) []byte {
		return startTimeRE.ReplaceAll(b, []byte(`,"started_at":"1999-01-02T03:04:05.000Z"`))
	},
	"not a time": func(b []byte) []byte { return startTimeRE.ReplaceAll(b, []byte(`,"started_at":"yesterday"`)) },
}

// withStart copies runID's journal from src into a new store, its run:start edited by edit.
func withStart(t *testing.T, src *Journal, runID string, edit func([]byte) []byte) *Journal {
	t.Helper()
	ctx := context.Background()
	dst := NewMemStore()
	for e, err := range src.store.Load(ctx, runID, -1) {
		if err != nil {
			t.Fatal(err)
		}
		data := e.Data
		if e.Name == runStartStep {
			if !startTimeRE.Match(data) {
				t.Fatalf("run:start holds no start time: %s", data)
			}
			data = edit(bytes.Clone(data))
		}
		if _, _, err := dst.Insert(ctx, runID, e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	return mustJournal(dst)
}

// loseOnce fails the first insert of the record named lose: the write a crash loses.
type loseOnce struct {
	Store
	lose string
}

var errLost = errors.New("lost write")

func (s *loseOnce) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == s.lose {
		s.lose = ""
		return Entry{}, false, errLost
	}
	return s.Store.Insert(ctx, runID, name, data)
}

func (s *loseOnce) Unwrap() Store { return s.Store }

// The start time is informational: a run whose run:start holds another start time, none (as
// an earlier version wrote it), or one that is not a time resumes exactly as it would have.
// Each case drives the run on from the same journal under each variant and compares what the
// drive does and journals: a resume after an approval, a halt (the at-most-once check), and a
// recovery pass.
func TestRunStart_StartedAtIsNotRead(t *testing.T) {
	ctx := context.Background()
	type outcome struct {
		text             string
		err              string
		lookups, refunds int
		recovered        int
	}
	cases := map[string]struct {
		setup func(t *testing.T) *Journal            // a journal of run "r", stopped mid-run
		drive func(t *testing.T, j *Journal) outcome // drives "r" on
	}{
		"resume after approval": {
			setup: func(t *testing.T) *Journal {
				j := memJournal()
				var n int
				_, err := mustNew(lookupRefund, j, refundTools(&n, &n, SingleApproval())).Run(ctx, "r", UserText("go"))
				if _, ok := errors.AsType[*ApprovalPending](err); !ok {
					t.Fatalf("setup: %v, want approval pending", err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				if err := Approve(ctx, j, "r", "c2", true); err != nil {
					t.Fatal(err)
				}
				res, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, SingleApproval())).Run(ctx, "r", UserText("go"))
				if err != nil {
					o.err = err.Error()
				} else {
					o.text = res.Message.Text()
				}
				return o
			},
		},
		"halt": {
			setup: func(t *testing.T) *Journal {
				s := &loseOnce{Store: NewMemStore(), lose: ToolResultStep("c2")}
				j := mustJournal(s)
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil)).Run(ctx, "r", UserText("go")); !errors.Is(err, errLost) {
					t.Fatalf("setup: %v, want the lost write", err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				_, err := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, nil)).Run(ctx, "r", UserText("go"))
				if _, ok := errors.AsType[*OutcomeUnknown](err); !ok {
					t.Fatalf("resume = %v, want *OutcomeUnknown", err)
				}
				o.err = fmt.Sprintf("%T", err)
				return o
			},
		},
		"recovery": {
			setup: func(t *testing.T) *Journal {
				s := &loseOnce{Store: NewMemStore(), lose: ToolResultStep("c1")}
				j := mustJournal(s)
				var n int
				if _, err := mustNew(lookupRefund, j, refundTools(&n, &n, nil)).Run(ctx, "r", UserText("go")); !errors.Is(err, errLost) {
					t.Fatalf("setup: %v, want the lost write", err)
				}
				return j
			},
			drive: func(t *testing.T, j *Journal) outcome {
				var o outcome
				a := mustNew(lookupRefund, j, refundTools(&o.lookups, &o.refunds, nil))
				n, err := Recover(ctx, j, ResumeAgent(a))
				if err != nil {
					t.Fatal(err)
				}
				o.recovered = n
				if done, err := IsComplete(ctx, j, "r"); err != nil || !done {
					t.Fatalf("after recovery: complete %v, %v", done, err)
				}
				return o
			},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src := c.setup(t)
			base := withStart(t, src, "r", startVariants["as written"])
			want := c.drive(t, base)
			for v, edit := range startVariants {
				j := withStart(t, src, "r", edit)
				if got := c.drive(t, j); got != want {
					t.Errorf("%s: start time %s: %+v, want %+v", name, v, got, want)
				}
				sameDrive(t, base, j, "r")
			}
		})
	}
}

// sameDrive fails unless a and b hold the same journal for runID, but for what differs between
// any two drives: salts, the start time, the recorded_at of the records that carry one alone (a
// single approval, an end marker), and each attempt marker's time and claim token.
func sameDrive(t *testing.T, a, b *Journal, runID string) {
	t.Helper()
	var js [2][]Record
	var presence [2][]bool
	for i, j := range []*Journal{a, b} {
		recs, err := j.History(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		for k := range recs {
			recs[k].salt, recs[k].raw = nil, nil
			recs[k].Result = withoutStartTime(t, recs[k])
			if recs[k].Kind == StepAttempt {
				recs[k].AttemptedAt, recs[k].claim = 0, ""
			}
		}
		presence[i] = stripRecordedAt(recs)
		js[i] = recs
	}
	if !slices.Equal(presence[0], presence[1]) {
		t.Fatalf("the journals differ in which records carry a recorded_at:\n %s\n %s", dumpRecords(js[0]), dumpRecords(js[1]))
	}
	if !reflect.DeepEqual(js[0], js[1]) {
		t.Fatalf("journals differ:\n %s\n %s", dumpRecords(js[0]), dumpRecords(js[1]))
	}
}

// A run:start written by an earlier version, with no start time, loads: RecordedStart and the
// recovery read decode it (the latter without the JSON decoder), Started reports none, and the
// run is driven to its end.
func TestRunStart_EarlierJournalLoads(t *testing.T) {
	ctx := context.Background()
	s := &loseOnce{Store: NewMemStore(), lose: ToolResultStep("c1")}
	var n int
	if _, err := mustNew(lookupRefund, mustJournal(s), refundTools(&n, &n, nil)).Run(ctx, "r", UserText("go")); !errors.Is(err, errLost) {
		t.Fatalf("setup: %v", err)
	}
	j := withStart(t, mustJournal(s.Store), "r", startVariants["none"])
	e, _, err := j.store.Get(ctx, "r", runStartStep)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(e.Data, []byte("started_at")) {
		t.Fatalf("the edited run:start still holds a start time: %s", e.Data)
	}
	if fast, ok := decodePlainStart(e.Data); !ok || fast.StartedAt != "" || fast.Kind != RunKindAgent {
		t.Fatalf("decodePlainStart = %+v, %v", fast, ok)
	}
	st, ok, err := RecordedStart(ctx, j, "r")
	if err != nil || !ok || st.StartedAt != "" || st.Input.Text() != "go" {
		t.Fatalf("RecordedStart = %+v, %v, %v", st, ok, err)
	}
	if _, ok := st.Started(); ok {
		t.Fatal("Started reports a time for a run:start that holds none")
	}
	var lookups, refunds int
	res, err := mustNew(lookupRefund, j, refundTools(&lookups, &refunds, nil)).Run(ctx, "r", UserText("go"))
	if err != nil || res.Message.Text() != "refunded" || lookups != 1 || refunds != 1 {
		t.Fatalf("Run = %v, %v; lookups %d refunds %d", res, err, lookups, refunds)
	}
	if st, _, _ := RecordedStart(ctx, j, "r"); st.StartedAt != "" {
		t.Fatalf("a later drive wrote a start time: %q", st.StartedAt)
	}
}
