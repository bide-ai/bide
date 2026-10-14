package runview

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

var errDisk = errors.New("disk on fire")

// flakyStore serves what it holds until failLoad is set, then every Load fails.
type flakyStore struct {
	agent.Store
	failLoad bool
}

func (s *flakyStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	if s.failLoad {
		return func(yield func(agent.Entry, error) bool) { yield(agent.Entry{}, errDisk) }
	}
	return s.Store.Load(ctx, runID, after)
}

func (s *flakyStore) Unwrap() agent.Store { return s.Store }

// TestLiveStoreErrors: a store that fails is a 500 naming the failure, on the poll and on the
// page, never a page of a run that is not there; a run with no records is a 404.
func TestLiveStoreErrors(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	m := agenttest.NewScriptedModel(agenttest.TextTurn("hi"))
	if _, err := agenttest.MustNew(m, agenttest.MustJournal(mem)).Run(ctx, "r1", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	fs := &flakyStore{Store: mem}
	h := Handler(Options{Source: JournalSource{J: agenttest.MustJournal(fs)}})

	if code, _ := get(t, h, "/run/live?id=nosuch&after=0"); code != http.StatusOK {
		t.Errorf("poll of an unknown run with nothing after the cursor = %d, want 200 (not grown)", code)
	}

	fs.failLoad = true
	if code, body := get(t, h, "/run/live?id=r1&after=0"); code != http.StatusInternalServerError || !strings.Contains(body, "disk on fire") {
		t.Errorf("poll over a failing store = %d %q, want 500 naming the failure", code, body)
	}
	if code, body := get(t, h, "/run?id=r1"); code != http.StatusInternalServerError || !strings.Contains(body, "disk on fire") {
		t.Errorf("page over a failing store = %d %q, want 500 naming the failure", code, body)
	}
}

// TestJournalSourceGrew: Grew is true exactly when a record follows the sequence, and reports a
// store error rather than "no".
func TestJournalSourceGrew(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	m := agenttest.NewScriptedModel(agenttest.TextTurn("hi"))
	if _, err := agenttest.MustNew(m, agenttest.MustJournal(mem)).Run(ctx, "r1", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	fs := &flakyStore{Store: mem}
	src := JournalSource{J: agenttest.MustJournal(fs)}
	_, seqs, err := src.HistorySeq(ctx, "r1")
	if err != nil || len(seqs) == 0 {
		t.Fatalf("HistorySeq = %v, %v", seqs, err)
	}
	last := seqs[len(seqs)-1]
	if grew, err := src.Grew(ctx, "r1", last-1); err != nil || !grew {
		t.Errorf("Grew(last-1) = %v, %v, want true", grew, err)
	}
	if grew, err := src.Grew(ctx, "r1", last); err != nil || grew {
		t.Errorf("Grew(last) = %v, %v, want false", grew, err)
	}
	fs.failLoad = true
	if grew, err := src.Grew(ctx, "r1", 0); !errors.Is(err, errDisk) || grew {
		t.Errorf("Grew over a failing store = %v, %v, want false and the error", grew, err)
	}
	if _, _, err := src.HistorySeq(ctx, "r1"); !errors.Is(err, errDisk) {
		t.Errorf("HistorySeq over a failing store: %v", err)
	}
}

// TestStaticStoreReadOnly: the store the readers run over refuses writes and serves the records
// it holds by name.
func TestStaticStoreReadOnly(t *testing.T) {
	ctx := context.Background()
	recs := []agent.Record{{Name: "a"}, {Name: "b"}}
	s := staticStore{recs: recs}
	if _, _, err := s.Insert(ctx, "r", "x", nil); !errors.Is(err, errReadOnly) {
		t.Errorf("Insert = %v, want errReadOnly", err)
	}
	if e, ok, err := s.Get(ctx, "r", "b"); err != nil || !ok || e.Seq != 2 {
		t.Errorf("Get(b) = %+v, %v, %v, want seq 2", e, ok, err)
	}
	if _, ok, _ := s.Get(ctx, "r", "zzz"); ok {
		t.Error("Get of a missing record reported it found")
	}
}
