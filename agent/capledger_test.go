package agent

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"iter"
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

// delayStore is a Store that waits a little before every operation, and longer before an insert, so
// that between any read a writer makes and its insert another writer's whole insert can happen.
type delayStore struct {
	Store
	n atomic.Int64
}

func (d *delayStore) pause(long bool) {
	k := d.n.Add(1)
	w := time.Duration(k%3) * 300 * time.Microsecond
	if long {
		w += 2 * time.Millisecond
	}
	time.Sleep(w)
}

func (d *delayStore) Insert(ctx context.Context, run, name string, data []byte) (Entry, bool, error) {
	d.pause(true)
	return d.Store.Insert(ctx, run, name, data)
}

func (d *delayStore) Get(ctx context.Context, run, name string) (Entry, bool, error) {
	d.pause(false)
	return d.Store.Get(ctx, run, name)
}

func (d *delayStore) Load(ctx context.Context, run string, after int64) iter.Seq2[Entry, error] {
	d.pause(false)
	return d.Store.Load(ctx, run, after)
}

// race runs n writers, each through a Journal of its own over one store that delays every operation (a
// new process each), against the same name with different bytes, and checks the one guarantee the
// ledger rests on (A1): exactly one insert wins, and every loser is handed the winner's bytes.
func race(t *testing.T, name string, write func(j *Journal, data []byte) (got []byte, inserted bool, err error)) {
	t.Helper()
	const n = 12
	st := &delayStore{Store: NewMemStore()}
	type result struct {
		got      []byte
		inserted bool
		err      error
		mine     []byte
	}
	res := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mine := []byte(fmt.Sprintf(`{"writer":%d}`, i))
			got, ins, err := write(newJournal(st), mine)
			res[i] = result{got, ins, err, mine}
		}()
	}
	wg.Wait()
	winners := 0
	var winner []byte
	for _, r := range res {
		if r.err != nil {
			t.Fatalf("%s: %v", name, r.err)
		}
		if r.inserted {
			winners++
			winner = r.mine
		}
	}
	if winners != 1 {
		t.Fatalf("%s: %d inserts won the name, want exactly one", name, winners)
	}
	for i, r := range res {
		if string(r.got) != string(winner) {
			t.Fatalf("%s: writer %d was handed %s, want the winner's %s", name, i, r.got, winner)
		}
	}
}

// The guarantee that matters, tested by its effect and not by the shape of the code: through Journal.putNew
// and through the adapter, with every operation delayed, many writers race one name and exactly one wins.
func TestCapLedger_InsertHasOneWinnerWhateverTheDelays(t *testing.T) {
	ctx := context.Background()
	for round := range 5 {
		name := fmt.Sprintf("slot:%d", round)
		race(t, "Journal.putNew "+name, func(j *Journal, data []byte) ([]byte, bool, error) {
			rec, ins, err := j.putNew(ctx, "@cap/x/y/USD/2026-10-22/z", name, Record{Kind: StepValue, Result: data})
			return rec.Result, ins, err
		})
		race(t, "capJournal.Insert "+name, func(j *Journal, data []byte) ([]byte, bool, error) {
			return capJournal{j}.Insert(ctx, "@cap/x/y/USD/2026-10-22/z", name, data)
		})
	}
}

// And through the ledger: slots reserved by journals over a store that delays every operation never
// overshoot, whatever the interleaving.
func TestCapLedger_ReserveOverADelayingStoreNeverOvershoots(t *testing.T) {
	st := &delayStore{Store: NewMemStore()}
	const n, limit = 20, 7
	var wg sync.WaitGroup
	var ok, refused atomic.Int32
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := newCapLedger(newJournal(st), capClock(), 0)
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
		t.Fatalf("%d reserved and %d refused, want %d and %d", ok.Load(), refused.Load(), limit, n-limit)
	}
}

// A cheap tripwire beside those tests, not the guarantee: capJournal.Insert makes exactly one call in all
// (to putNew), and the only Store the package hands to the ledger is capJournal{j}, so no other adapter
// or decorator is in the path.
func TestCapLedger_AdapterTripwire(t *testing.T) {
	var insertCalls []string
	var stores []string
	for file, f := range parseAgentPackage(t) {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if ok && file == "capledger.go" && fn.Recv != nil && fn.Name.Name == "Insert" {
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok {
						insertCalls = append(insertCalls, types.ExprString(c.Fun))
					}
					return true
				})
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if types.ExprString(lit.Type) != "capledger.Config" {
				return true
			}
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok && types.ExprString(kv.Key) == "Store" {
					desc := types.ExprString(kv.Value)
					if c, ok := kv.Value.(*ast.CompositeLit); ok && len(c.Elts) == 1 {
						desc = types.ExprString(c.Type) + "{" + types.ExprString(c.Elts[0]) + "}"
					}
					stores = append(stores, file+": "+desc)
				}
			}
			return true
		})
	}
	if len(insertCalls) != 1 || insertCalls[0] != "c.j.putNew" {
		t.Fatalf("capJournal.Insert makes the calls %v, want exactly c.j.putNew", insertCalls)
	}
	if len(stores) != 1 || stores[0] != "capledger.go: capJournal{j}" {
		t.Fatalf("the Store handed to the ledger: %v, want exactly capJournal{j} in newCapLedger", stores)
	}
}
