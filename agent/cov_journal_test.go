package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"
)

type covJournalOpt struct{ err error }

func (o covJournalOpt) applyJournal(*journalConfig) error { return o.err }

// NewJournal refuses a nil option, and returns the error an option reports.
func TestNewJournal_OptionErrors(t *testing.T) {
	if _, err := NewJournal(NewMemStore(), nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("nil option = %v; want ErrConfig", err)
	}
	want := errors.New("bad option")
	if _, err := NewJournal(NewMemStore(), covJournalOpt{}, covJournalOpt{want}); !errors.Is(err, want) {
		t.Fatalf("failing option = %v; want its error", err)
	}
	if j, err := NewJournal(NewMemStore(), covJournalOpt{}); err != nil || j == nil {
		t.Fatalf("passing option = %v, %v", j, err)
	}
}

// A nil journal is refused as ErrConfig by checkJournal and has no identity.
func TestJournal_NilJournal(t *testing.T) {
	var j *Journal
	if err := checkJournal(j); !errors.Is(err, ErrConfig) {
		t.Fatalf("checkJournal(nil) = %v; want ErrConfig", err)
	}
	if _, ok := j.identity(); ok {
		t.Fatal("a nil journal has an identity")
	}
	if err := checkJournal(memJournal()); err != nil {
		t.Fatalf("checkJournal(journal) = %v", err)
	}
}

// seedHeader stores a header entry holding data as run r's first entry, the way an older or
// foreign writer might.
func seedHeader(t *testing.T, s Store, r string, data string) {
	t.Helper()
	if _, _, err := s.Insert(context.Background(), r, headerStep, []byte(data)); err != nil {
		t.Fatal(err)
	}
}

// A run whose header names a format this version does not support, or is not a header at all, is
// refused by every entry point before a step's function runs or anything is written.
func TestJournal_UnreadableHeaderRefusesEverything(t *testing.T) {
	ctx := context.Background()
	future := `{"name":"` + headerStep + `","kind":"` + string(StepHeader) + `","format":"bide-journal-999"}`
	cases := map[string]struct {
		first    string // data of the run's first entry, stored as the header
		versions bool   // want a *JournalVersionError (else ErrStorage)
	}{
		"future format":     {future, true},
		"not a header":      {`{"name":"x","kind":"value"}`, false},
		"not json":          {`<<<`, false},
		"header wrong kind": {`{"name":"` + headerStep + `","kind":"value","format":"x"}`, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := NewMemStore()
			seedHeader(t, s, "r", c.first)
			j := mustJournal(s)
			check := func(what string, err error) {
				t.Helper()
				var ve *JournalVersionError
				switch {
				case err == nil:
					t.Errorf("%s succeeded on an unreadable header", what)
				case c.versions && !errors.As(err, &ve):
					t.Errorf("%s = %v; want a *JournalVersionError", what, err)
				case !c.versions && !errors.Is(err, ErrStorage):
					t.Errorf("%s = %v; want ErrStorage", what, err)
				}
			}
			ran := false
			fn := func(context.Context) (Record, error) {
				ran = true
				return Record{Kind: StepValue, Result: []byte(`1`)}, nil
			}
			_, err := j.do(ctx, "r", "s", fn)
			check("do", err)
			_, err = j.doFresh(ctx, "r", "s", fn)
			check("doFresh", err)
			_, err = j.put(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)})
			check("put", err)
			_, _, err = j.putNew(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)})
			check("putNew", err)
			_, err = j.History(ctx, "r")
			check("History", err)
			_, _, err = j.Get(ctx, "r", headerStep)
			check("Get", err)
			if c.versions {
				if f, err := j.Format(ctx, "r"); err != nil || f != "bide-journal-999" {
					t.Errorf("Format = %q, %v; want the format reported, not refused", f, err)
				}
			} else {
				_, err = j.Format(ctx, "r")
				check("Format", err)
			}
			if ran {
				t.Error("a step's function ran against an unreadable run")
			}
		})
	}
}

// Format of a run whose first entry is not a header is a JournalVersionError, of an empty run is
// "", and of a run this version wrote is the current format.
func TestJournal_Format(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	j := mustJournal(s)
	if f, err := j.Format(ctx, "none"); f != "" || err != nil {
		t.Fatalf("Format of an empty run = %q, %v", f, err)
	}
	if _, err := j.put(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)}); err != nil {
		t.Fatal(err)
	}
	if f, err := j.Format(ctx, "r"); err != nil || f != JournalFormat {
		t.Fatalf("Format = %q, %v; want %q", f, err, JournalFormat)
	}
	if _, _, err := s.Insert(ctx, "raw", "step", []byte(`{"name":"step","kind":"value"}`)); err != nil {
		t.Fatal(err)
	}
	var ve *JournalVersionError
	if _, err := mustJournal(s).Format(ctx, "raw"); !errors.As(err, &ve) || ve.Found != "" {
		t.Fatalf("Format of a run with no header first = %v; want a *JournalVersionError with no format found", err)
	}
}

// A store that cannot write the header makes every write fail, and the step's function does not
// run: a run's format is settled before a side effect could fire.
func TestJournal_HeaderWriteFailureStopsBeforeTheEffect(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(covInsertFail{NewMemStore(), headerStep})
	ran := false
	fn := func(context.Context) (Record, error) {
		ran = true
		return Record{Kind: StepValue, Result: []byte(`1`)}, nil
	}
	if _, err := j.do(ctx, "r", "s", fn); !errors.Is(err, ErrStorage) {
		t.Errorf("do = %v; want ErrStorage", err)
	}
	if _, err := j.doFresh(ctx, "r", "s", fn); !errors.Is(err, ErrStorage) {
		t.Errorf("doFresh = %v; want ErrStorage", err)
	}
	if _, err := j.put(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)}); !errors.Is(err, ErrStorage) {
		t.Errorf("put = %v; want ErrStorage", err)
	}
	if _, _, err := j.putNew(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)}); !errors.Is(err, ErrStorage) {
		t.Errorf("putNew = %v; want ErrStorage", err)
	}
	if ran {
		t.Error("a step's function ran though the run's header could not be written")
	}
}

// A record write the store refuses is ErrStorage, and the same step is recorded by a later try.
func TestJournal_PutNewInsertFailure(t *testing.T) {
	ctx := context.Background()
	st := &failOnceInsert{Store: NewMemStore(), sub: "s"}
	j := mustJournal(st)
	if _, _, err := j.putNew(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)}); !errors.Is(err, ErrStorage) {
		t.Fatalf("putNew over a failing store = %v; want ErrStorage", err)
	}
	rec, inserted, err := j.putNew(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)})
	if err != nil || !inserted || string(rec.Result) != `1` {
		t.Fatalf("putNew on the retry = %+v, %v, %v", rec, inserted, err)
	}
}

// failOnceInsert fails the first Insert of an entry named sub.
type failOnceInsert struct {
	Store
	sub  string
	done bool
}

func (f *failOnceInsert) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == f.sub && !f.done {
		f.done = true
		return Entry{}, false, errCovBlip
	}
	return f.Store.Insert(ctx, runID, name, data)
}

// A record the codec cannot encode is ErrStorage, naming the step, and nothing is recorded.
func TestJournal_UnencodableRecord(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	bad := Record{Kind: StepValue, Result: []byte(`{not json`)}
	if _, _, err := j.putNew(ctx, "r", "s", bad); !errors.Is(err, ErrStorage) {
		t.Errorf("putNew = %v; want ErrStorage", err)
	}
	if _, err := j.do(ctx, "r", "t", func(context.Context) (Record, error) { return bad, nil }); !errors.Is(err, ErrStorage) {
		t.Errorf("do = %v; want ErrStorage", err)
	}
	for _, n := range []string{"s", "t"} {
		if _, ok, _ := j.Get(ctx, "r", n); ok {
			t.Errorf("an unencodable record was stored as %q", n)
		}
	}
}

// A stored record that does not decode is reported as ErrStorage by Get and History, not skipped.
func TestJournal_UndecodableStoredRecord(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	j := mustJournal(s)
	if _, err := j.put(ctx, "r", "ok", Record{Kind: StepValue, Result: []byte(`1`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "r", "bad", []byte(`{"name":`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Get(ctx, "r", "bad"); !errors.Is(err, ErrStorage) {
		t.Errorf("Get = %v; want ErrStorage", err)
	}
	recs, err := j.History(ctx, "r")
	if !errors.Is(err, ErrStorage) || recs != nil {
		t.Errorf("History = %v, %v; want nil and ErrStorage", recs, err)
	}
	var got int
	for _, err := range j.Records(ctx, "r") {
		if err == nil {
			got++
		}
	}
	if got != 2 { // the header and "ok", before the bad one
		t.Errorf("Records yielded %d good records before the bad one; want 2", got)
	}
}

// Stopping Records early stops the load, and a failing Load is yielded once as ErrStorage.
func TestJournal_RecordsStopsAndLoadFaults(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	for i := range 3 {
		if _, err := j.put(ctx, "r", fmt.Sprintf("s%d", i), Record{Kind: StepValue, Result: []byte(`1`)}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, err := range j.Records(ctx, "r") {
		if err != nil {
			t.Fatal(err)
		}
		if n++; n == 2 {
			break
		}
	}
	if n != 2 {
		t.Fatalf("early break read %d records", n)
	}
	lf := mustJournal(loadFails{NewMemStore()})
	for _, err := range lf.Records(ctx, "r") {
		if !errors.Is(err, ErrStorage) {
			t.Fatalf("Records over a failing Load = %v; want ErrStorage", err)
		}
	}
	if _, err := lf.Format(ctx, "r"); !errors.Is(err, ErrStorage) {
		t.Fatalf("Format over a failing Load = %v; want ErrStorage", err)
	}
}

// loadFails yields an error from every Load.
type loadFails struct{ Store }

func (loadFails) Load(context.Context, string, int64) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) { yield(Entry{}, errCovBlip) }
}

// The claim memo is bounded: past maxPendingClaims ids the oldest keys are dropped (a dropped id
// only costs a halt a later claim would have avoided), the newest stay, and keys already taken
// do not pile up in its order list.
func TestClaimMemo_IsBounded(t *testing.T) {
	c := &claimMemo{m: map[flightKey]map[string]struct{}{}}
	key := func(i int) flightKey { return flightKey{nil, "r", fmt.Sprintf("k%d", i)} }
	for i := 0; i < maxPendingClaims+10; i++ {
		c.remember(key(i), "id")
	}
	if c.n > maxPendingClaims {
		t.Fatalf("memo holds %d ids; the bound is %d", c.n, maxPendingClaims)
	}
	if got := c.takeAll(key(0)); len(got) != 0 {
		t.Errorf("the oldest key survived the bound: %v", got)
	}
	if got := c.takeAll(key(maxPendingClaims + 9)); len(got) != 1 {
		t.Errorf("the newest key was dropped: %v", got)
	}
	// Keys taken as soon as they are remembered leave stale entries in order; they are compacted.
	d := &claimMemo{m: map[flightKey]map[string]struct{}{}}
	for i := 0; i < 2*maxPendingClaims+5; i++ {
		d.remember(key(i), "id")
		d.takeAll(key(i))
	}
	if len(d.order) > 2*maxPendingClaims {
		t.Errorf("order list holds %d keys though none is live", len(d.order))
	}
	if d.n != 0 {
		t.Errorf("memo holds %d ids after all were taken", d.n)
	}
}
