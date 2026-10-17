package audit_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

func forge(t *testing.T, names ...string) (*agent.MemStore, *agent.MemStore) {
	t.Helper()
	ctx := context.Background()
	s, _ := p11GovernedRun(t)
	if _, _, err := s.Insert(ctx, "gov", "run:complete", mustEntry(t, "run:complete", agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"ok"`)})); err != nil {
		t.Fatal(err)
	}
	f := agent.NewMemStore()
	for e, err := range s.Load(ctx, "gov", -1) {
		if err != nil {
			t.Fatal(err)
		}
		data := e.Data
		for _, n := range names {
			if e.Name == n {
				data = []byte(fmt.Sprintf(`{"redacted":{"leaf_hash":"%s","at_ms":1}}`, hex.EncodeToString(agent.LeafHash(e.Data))))
			}
		}
		if _, _, err := f.Insert(ctx, "gov", e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	return s, f
}

func TestBreak_ReadersOnForgedProtectedTombstones(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"run:complete", "@journal", "audit:policy"} {
		s, f := forge(t, name)
		j, fj := agenttest.MustJournal(s), agenttest.MustJournal(f)
		r1, e1 := audit.Root(ctx, j, "gov")
		r2, e2 := audit.Root(ctx, fj, "gov")
		t.Logf("[%s] root equal=%v errs=%v %v", name, bytes.Equal(r1, r2), e1, e2)
		st, err := agent.Status(ctx, fj, "gov")
		t.Logf("[%s] Status=%+v err=%v", name, st, err)
		x, err := audit.ExportJournal(ctx, fj, "gov")
		if err == nil {
			_, err = x.Journal()
		}
		t.Logf("[%s] export/Journal err=%v", name, err)
		signer := p11Signers(t)["ed25519"]
		v := p11Verifier(t, signer)
		pkg, err := audit.Evidence(ctx, fj, "gov", signer, p11Now(), audit.WithAllToolCalls())
		if err != nil {
			t.Logf("[%s] Evidence err=%v", name, err)
			continue
		}
		rep, err := pkg.Verify(v)
		t.Logf("[%s] Evidence.Verify err=%v ok=%+v", name, err, rep)
	}
}
