package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

func TestNotifyTrail_Contract(t *testing.T) {
	agenttest.NotifyTrailContract(t, func() func() agent.NotifyTrail {
		es := audit.NewMemEventStore()
		return func() agent.NotifyTrail { return audit.NewNotifyTrail(es) }
	})
}

// lossyStore commits an append and then reports an error, as a store does when its reply is lost.
type lossyStore struct {
	audit.EventStore
	commitThenFail bool
	fail           bool
}

func (l *lossyStore) Append(ctx context.Context, runID string, seq int, leaf []byte) error {
	if l.fail {
		return errors.New("store down")
	}
	err := l.EventStore.Append(ctx, runID, seq, leaf)
	if err == nil && l.commitThenFail {
		return errors.New("reply lost")
	}
	return err
}

func notifyRec(k agent.NotifyRecordKind, key string) agent.NotifyRecord {
	return agent.NotifyRecord{Kind: k, Key: key, RunID: "r1", At: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC), Holder: "p1"}
}

// A lost reply: the append committed, the caller saw an error. Retrying the same record must be a
// no-op (it is our own write), not a conflict with ourselves, and must not duplicate the record.
func TestNotifyTrail_LostReplyIsOurOwnWrite(t *testing.T) {
	ctx := context.Background()
	ls := &lossyStore{EventStore: audit.NewMemEventStore(), commitThenFail: true}
	tr := audit.NewNotifyTrail(ls)
	r := notifyRec(agent.NotifyClaimed, "k")
	if err := tr.Append(ctx, "t", 0, r); err != nil {
		t.Fatalf("an append that committed but lost its reply must be recognised as ours: %v", err)
	}
	ls.commitThenFail = false
	if err := tr.Append(ctx, "t", 0, r); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got, _ := tr.Load(ctx, "t"); len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
}

// A real failure (nothing committed) is an error but not a conflict: the caller must not conclude
// that someone else holds the claim.
func TestNotifyTrail_StoreFailureIsNotAConflict(t *testing.T) {
	ls := &lossyStore{EventStore: audit.NewMemEventStore(), fail: true}
	err := audit.NewNotifyTrail(ls).Append(context.Background(), "t", 0, notifyRec(agent.NotifyClaimed, "k"))
	if err == nil || errors.Is(err, agent.ErrTrailConflict) {
		t.Fatalf("err = %v, want a non-conflict error", err)
	}
}

// Another process's different record at the position is the loser's conflict, even when the store
// also reports an error for it.
func TestNotifyTrail_OtherWritersRecordIsAConflict(t *testing.T) {
	ctx := context.Background()
	es := audit.NewMemEventStore()
	a, b := audit.NewNotifyTrail(es), audit.NewNotifyTrail(es)
	ra, rb := notifyRec(agent.NotifyClaimed, "k"), notifyRec(agent.NotifyClaimed, "k")
	rb.Holder = "p2"
	if err := a.Append(ctx, "t", 0, ra); err != nil {
		t.Fatal(err)
	}
	if err := b.Append(ctx, "t", 0, rb); !errors.Is(err, agent.ErrTrailConflict) {
		t.Fatalf("err = %v, want ErrTrailConflict", err)
	}
}

// The trail is an ordinary event trail: its leaves commit under a Merkle root and an inclusion
// proof for a record verifies from the record and proof alone.
func TestNotifyTrail_RecordsAreProvableEventLeaves(t *testing.T) {
	ctx := context.Background()
	es := audit.NewMemEventStore()
	tr := audit.NewNotifyTrail(es)
	recs := []agent.NotifyRecord{notifyRec(agent.NotifyObserved, "k"), notifyRec(agent.NotifyClaimed, "k"), notifyRec(agent.NotifyAcked, "k")}
	recs[1].At = recs[1].At.In(time.FixedZone("x", 7200)) // written in another zone, committed in UTC
	for i, r := range recs {
		if err := tr.Append(ctx, "t", i, r); err != nil {
			t.Fatal(err)
		}
	}
	log, err := audit.LoadEventLog(ctx, es, "t")
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range recs {
		proof, err := log.Prove(i)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.VerifyEventInclusion(log.Root(), r.Normalized(), proof); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		// A different record does not verify under the same proof.
		other := r.Normalized()
		other.Holder = "mallory"
		if err := audit.VerifyEventInclusion(log.Root(), other, proof); err == nil {
			t.Fatalf("record %d: a changed record verified", i)
		}
	}
}

// A trail that holds a leaf which is not a notify record is refused, not skipped.
func TestNotifyTrail_LoadRefusesForeignLeaves(t *testing.T) {
	ctx := context.Background()
	es := audit.NewMemEventStore()
	if err := audit.PersistEvent(ctx, es, "t", 0, agent.TurnStarted{Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.NewNotifyTrail(es).Load(ctx, "t"); err == nil {
		t.Fatal("a non-notify leaf was accepted")
	}
}
