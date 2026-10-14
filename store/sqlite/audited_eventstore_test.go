package sqlite

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/eventstoretest"
)

// The audited wrapper keeps the EventStore contract on SQLite, across handles that race.
func TestSQLite_AuditedEventStoreKeepsTheContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	_, priv, _ := ed25519.GenerateKey(nil)
	log := audit.NewMemAnchorLog()
	eventstoretest.Run(t, func(t *testing.T) audit.EventStore {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		a, err := audit.NewAuditedEventStore(s.EventStore(), audit.Ed25519Signer{Priv: priv}, log)
		if err != nil {
			t.Fatal(err)
		}
		return a
	})
}

// Two processes' worth of audited handles on one file append one notification trail by racing for
// positions, each anchoring what it wrote to the same log. Every head published, whichever handle
// published it, commits to a prefix of the trail the file holds at the end.
func TestSQLite_AuditedEventStoreHeadsAreAlwaysAPrefixOfTheTrail(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	_, priv, _ := ed25519.GenerateKey(nil)
	log := audit.NewMemAnchorLog()
	const trail = "notify:r1"
	const handles, perHandle = 3, 8

	var wg sync.WaitGroup
	var audited []*audit.AuditedEventStore
	for h := range handles {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		a, err := audit.NewAuditedEventStore(s.EventStore(), audit.Ed25519Signer{Priv: priv}, log)
		if err != nil {
			t.Fatal(err)
		}
		audited = append(audited, a)
		a.OnError(func(tr string, err error) { t.Errorf("anchor %s: %v", tr, err) })
		tr := audit.NewNotifyTrail(a)
		wg.Go(func() {
			for i := range perHandle {
				for {
					recs, err := tr.Load(ctx, trail)
					if err != nil {
						t.Error(err)
						return
					}
					err = tr.Append(ctx, trail, len(recs), agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: fmt.Sprintf("h%d-%d", h, i), RunID: "r1", At: time.Now(), Holder: fmt.Sprintf("h%d", h)})
					if err == nil {
						break
					}
					if !errors.Is(err, agent.ErrTrailConflict) {
						t.Error(err)
						return
					}
				}
			}
		})
	}
	wg.Wait()
	for _, a := range audited {
		if err := a.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	leaves, err := s.EventStore().Load(ctx, trail)
	if err != nil || len(leaves) != handles*perHandle {
		t.Fatalf("%d leaves, %v", len(leaves), err)
	}
	if log.Len() == 0 {
		t.Fatal("nothing was anchored")
	}
	for _, e := range log.Entries() {
		mem := audit.NewMemEventStore()
		for i, l := range leaves[:e.STH.Size] {
			if err := mem.Append(ctx, trail, i, l); err != nil {
				t.Fatal(err)
			}
		}
		want, err := audit.SignNotifyTrail(ctx, mem, trail, audit.Ed25519Signer{Priv: priv}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want.Root, e.STH.Root) {
			t.Fatalf("head %d (size %d) does not commit to the first %d leaves of the stored trail", e.Seq, e.STH.Size, e.STH.Size)
		}
	}
}
