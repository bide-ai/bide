package agent

import (
	"context"
	"go/ast"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/capledger"
)

var capDay = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func capRule() capledger.Rule {
	return capledger.Rule{Namespace: "prod", Name: "refund_total", Currency: "USD", Kind: capledger.Day}
}

func capReq(attempt string, amount, limit int64) capledger.Request {
	return capledger.Request{Rule: capRule(), KeyDigest: capledger.KeyDigest(""), RunID: "r-" + attempt, RootRunID: "r",
		ToolUseID: attempt, AttemptID: "1", Marked: true, Amount: amount, Limit: limit}
}

func capClock() func() time.Time { return func() time.Time { return capDay } }

func TestCapLedger_RunPrefixIsTheReservedPrefix(t *testing.T) {
	if capledger.RunPrefix != capRunPrefix {
		t.Fatalf("capledger.RunPrefix = %q, capRunPrefix = %q", capledger.RunPrefix, capRunPrefix)
	}
}

// A ledger run is an ordinary journal run: a header first, then the slots, readable by History, and
// reserved: IsCapRun and IsReservedRun hold, so Runs consumers skip it. The attempt's intent and
// reserve records are in the run that made the call, under the reserved prefixes.
func TestCapLedger_LedgerIsAReservedJournalRun(t *testing.T) {
	ctx := context.Background()
	j := newJournal(NewMemStore())
	l := newCapLedger(j, capClock(), 0)
	o, err := l.Reserve(ctx, capReq("a1", 10, 100))
	if err != nil {
		t.Fatal(err)
	}
	if !IsCapRun(o.Ledger) || !IsReservedRun(o.Ledger) {
		t.Fatalf("ledger run %q is not reserved", o.Ledger)
	}
	recs, err := j.History(ctx, o.Ledger)
	if err != nil {
		t.Fatalf("History of the ledger run: %v", err)
	}
	var names []string
	for _, r := range recs {
		names = append(names, r.Name)
	}
	if len(names) != 2 || names[1] != "slot:0" {
		t.Fatalf("ledger run entries = %v, want the header and slot:0", names)
	}
	for _, name := range []string{"cap-intent:r%2Fa1%2F1", "cap-reserve:r%2Fa1%2F1"} {
		if _, ok, err := j.Get(ctx, "r-a1", name); err != nil || !ok {
			t.Fatalf("run r-a1 holds no %s (%v)", name, err)
		}
	}
	idx, err := j.History(ctx, l.IndexRun(capRule()))
	if err != nil || len(idx) != 2 || !strings.HasPrefix(idx[1].Name, "win:2026-10-22") {
		t.Fatalf("index run = %v, %v", idx, err)
	}
	// Runs lists the ledger and the index, and IsReservedRun is what every consumer skips them by.
	reserved := 0
	var ids []string
	for id, err := range j.Store().(Lister).Runs(ctx, RunFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if IsReservedRun(id) {
			reserved++
		}
	}
	if reserved != 2 {
		t.Fatalf("runs %v: %d reserved, want the ledger and the index", ids, reserved)
	}
}

// C2 at the agent level: goroutines over journals on one store (each a process's own cache).
func TestCapLedger_ConcurrentJournalsNeverOvershoot(t *testing.T) {
	mem := NewMemStore()
	const n, limit = 48, 16
	var wg sync.WaitGroup
	var ok, refused atomic.Int32
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := newCapLedger(newJournal(mem), capClock(), 0)
			_, err := l.Reserve(context.Background(), capReq(fmt.Sprintf("a%d", i), 1, limit))
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, capledger.ErrOverLimit):
				refused.Add(1)
			default:
				t.Errorf("a%d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != limit || refused.Load() != n-limit {
		t.Fatalf("%d reserved, %d refused; want %d and %d", ok.Load(), refused.Load(), limit, n-limit)
	}
}

// C4 at the agent level: a crash at every write the journal makes (the headers, the index, the intent,
// the slot, the audit record), then a new process reserves the attempt: it holds exactly one slot.
func TestCapLedger_CrashAtEveryWrite(t *testing.T) {
	ctx := context.Background()
	for crashAt := 1; crashAt <= 9; crashAt++ {
		t.Run(fmt.Sprintf("crash-%d", crashAt), func(t *testing.T) {
			mem := newJournal(NewMemStore())
			crashed := capClockLedger(crashJournal(mem, crashAt))
			if _, err := crashed.Reserve(ctx, capReq("a1", 10, 100)); err != nil && !errors.Is(err, errCrash) {
				t.Fatalf("crashed reservation: %v", err)
			}
			l := newCapLedger(mem, capClock(), 0)
			o, err := l.Reserve(ctx, capReq("a1", 10, 100))
			if err != nil {
				t.Fatal(err)
			}
			live, err := l.Live(ctx, o.Ledger)
			if err != nil || len(live) != 1 || live[0].Tool != "a1" || live[0].CumAfter != 10 {
				t.Fatalf("live = %+v, %v; want a1's one slot", live, err)
			}
		})
	}
}

func capClockLedger(j *Journal) *capledger.Ledger { return newCapLedger(j, capClock(), 0) }

// The adapter's put-new is the store's single-winner insert and nothing else: capJournal.Insert makes
// exactly one call, to the journal's putNew (one store Insert after the header check), and reads nothing
// first. A read followed by a write would be a check-then-act the store's A1 does not cover, and model
// 17's guarantee (and TestReserve_C2FailsWithANonAtomicInsert in internal/capledger) rests on A1.
func TestCapLedger_AdapterInsertIsOneAtomicPutNew(t *testing.T) {
	var calls []string
	for file, f := range parseAgentPackage(t) {
		if file != "capledger.go" {
			continue
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "Insert" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
						calls = append(calls, sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
	if len(calls) != 1 || calls[0] != "putNew" {
		t.Fatalf("capJournal.Insert calls %v, want exactly putNew", calls)
	}
}
