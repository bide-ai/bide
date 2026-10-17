package audit_test

// A tombstone at a name a redaction never replaces (the end marker, the audit leaves, the saga
// records) was not written by a redaction. It keeps the leaf hash, so roots and signed heads still
// verify, which is why every reader must report it rather than read the run in some state.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// forgedTombstones returns a copy of the governed run "gov" (with run:complete) in which each named
// entry is replaced, by hand, with a tombstone that names it and keeps its correct leaf hash.
func forgedTombstones(t *testing.T, names ...string) (orig, forged *agent.MemStore) {
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
				tomb, err := agent.NewTombstone(n, e.Data, time.UnixMilli(1))
				if err != nil {
					t.Fatal(err)
				}
				data = tomb
			}
		}
		if _, _, err := f.Insert(ctx, "gov", e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	return s, f
}

func TestRedactionCorrupt_EveryReaderFailsClosed(t *testing.T) {
	ctx := context.Background()
	signer := p11Signers(t)["ed25519"]
	for _, name := range []string{"run:complete", "audit:policy:D1", "@journal"} {
		orig, forged := forgedTombstones(t, name)
		if name == "audit:policy:D1" {
			// the governed run's policy record: find its real name
			for e := range orig.Load(ctx, "gov", -1) {
				if strings.HasPrefix(e.Name, "audit:") {
					name = e.Name
					break
				}
			}
			orig, forged = forgedTombstones(t, name)
		}
		fj := agenttest.MustJournal(forged)
		if _, err := audit.Root(ctx, fj, "gov"); !errors.Is(err, agent.ErrStorage) {
			t.Errorf("%s: Root = %v, want ErrStorage", name, err)
		}
		if _, err := agent.Status(ctx, fj, "gov"); !errors.Is(err, agent.ErrStorage) {
			t.Errorf("%s: Status = %v, want ErrStorage", name, err)
		}
		if _, err := audit.ExportJournal(ctx, fj, "gov"); !errors.Is(err, agent.ErrStorage) {
			t.Errorf("%s: ExportJournal = %v, want ErrStorage", name, err)
		}
		if _, err := audit.Evidence(ctx, fj, "gov", signer, p11Now(), audit.WithAllToolCalls()); err == nil {
			t.Errorf("%s: Evidence was built over a journal with a tombstone at a protected name", name)
		}
		if _, err := audit.NewTreeHead(ctx, fj, "gov", p11Now()); !errors.Is(err, agent.ErrStorage) {
			t.Errorf("%s: NewTreeHead = %v, want ErrStorage", name, err)
		}
	}
}

// An export edited after the fact, to put a named tombstone where an end marker was, is refused
// when it is read back.
func TestRedactionCorrupt_ForgedExportIsRefused(t *testing.T) {
	ctx := context.Background()
	orig, _ := forgedTombstones(t)
	x, err := audit.ExportJournal(ctx, agenttest.MustJournal(orig), "gov")
	if err != nil {
		t.Fatal(err)
	}
	last := len(x.Records) - 1 // run:complete
	tomb, err := agent.NewTombstone("run:complete", x.Records[last], time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	x.Records[last] = tomb
	if _, err := x.Journal(); !errors.Is(err, audit.ErrMalformed) {
		t.Fatalf("Journal() of an export with a tombstoned run:complete = %v, want ErrMalformed", err)
	}
}

// A tombstone's name is not under the leaf hash: whoever holds an export can rename it without
// breaking any root or signature. So a consumer that acts on the name labels it, and the rename
// changes nothing the leaf hash or a head attests.
func TestRedactionName_IsNotAttestedInAnExport(t *testing.T) {
	ctx := context.Background()
	s := agent.NewMemStore()
	j := agenttest.MustJournal(s)
	if _, err := j.Step(ctx, "r", "x", func(context.Context) (string, error) { return "x", nil }); err != nil {
		t.Fatal(err)
	}
	for name, kind := range map[string]agent.StepKind{"approval:call1": agent.StepApproval, "run:complete": agent.StepValue} {
		if _, _, err := s.Insert(ctx, "r", name, mustEntry(t, name, agent.Record{Kind: kind, Approved: true, Result: json.RawMessage(`"ok"`)})); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := agent.PlanRedaction(ctx, j, "r", agent.RedactionSelector{Payloads: true, IncludeApprovals: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Redact(ctx, j, plan, time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	x, err := audit.ExportJournal(ctx, j, "r")
	if err != nil {
		t.Fatal(err)
	}
	idx := -1
	for i, b := range x.Records {
		if n, ok := agent.TombstoneName(b); ok && n == "approval:call1" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("no tombstone named approval:call1 in the export")
	}
	before, _ := agent.TombstoneLeafHash(x.Records[idx])
	x.Records[idx] = bytes.Replace(x.Records[idx], []byte(`"name":"approval:call1"`), []byte(`"name":"approval:other"`), 1)
	after, ok := agent.TombstoneLeafHash(x.Records[idx])
	if !ok || !bytes.Equal(before, after) {
		t.Fatalf("renaming a tombstone changed its leaf hash: %x to %x (%v)", before, after, ok)
	}
	recs, err := x.Journal()
	if err != nil {
		t.Fatal(err)
	}
	signer := p11Signers(t)["ed25519"]
	rep, err := audit.CheckDecisionCoverage("r", recs, nil, p11Verifier(t, signer), audit.CoverageBound{MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Decisions) != 1 || rep.Decisions[0].Step != "approval:other" || rep.Decisions[0].Note != audit.NoteRedacted {
		t.Fatalf("decisions = %+v; the renamed tombstone must be listed under its unattested name with the note %q", rep.Decisions, audit.NoteRedacted)
	}
	if !strings.Contains(audit.NoteRedacted, "not attested") {
		t.Errorf("NoteRedacted = %q does not say the name is not attested", audit.NoteRedacted)
	}
}
