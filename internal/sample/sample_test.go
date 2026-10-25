package sample

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// dump is every entry of every run the sample wrote, in order, as text.
func dump(t *testing.T, s agent.Store, res *Result) string {
	t.Helper()
	var b strings.Builder
	for _, r := range res.Runs {
		for e, err := range s.Load(context.Background(), r.ID, -1) {
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\n", r.ID, e.Name, Unsalted(e.Data))
		}
	}
	return b.String()
}

func write(t *testing.T) (string, *Result) {
	t.Helper()
	s := agent.NewMemStore()
	res, err := Write(context.Background(), s, "")
	if err != nil {
		t.Fatal(err)
	}
	return dump(t, s, res), res
}

// TestDeterministic: two calls write the same entries but for their salts, and the same keys.
func TestDeterministic(t *testing.T) {
	a, ra := write(t)
	b, rb := write(t)
	if a != b {
		la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
		for i := range la {
			if i >= len(lb) || la[i] != lb[i] {
				t.Fatalf("the journals differ at entry %d:\n%s\n%s", i, la[i], lb[min(i, len(lb)-1)])
			}
		}
		t.Fatal("the journals differ")
	}
	// The keys and the run set are the same; the evidence holds salted digests, so it differs.
	ra.Evidence, rb.Evidence = nil, nil
	ja, _ := json.Marshal(ra)
	jb, _ := json.Marshal(rb)
	if string(ja) != string(jb) {
		t.Fatal("the keys or runs differ between calls")
	}
}

// TestCapLedger: the two evidence runs' refunds are counted in one day's cap ledger, which bide audit caps
// reads.
func TestCapLedger(t *testing.T) {
	ctx := context.Background()
	s := agent.NewMemStore()
	if _, err := Write(ctx, s, ""); err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	l := journalhook.CapLedger(j, time.Now, 0)
	var ledgers []string
	for id := range s.Runs(ctx, agent.RunFilter{Prefix: "@cap/"}) {
		if strings.Count(id, "/") == 5 {
			ledgers = append(ledgers, id)
		}
	}
	if len(ledgers) != 1 {
		t.Fatalf("ledger runs = %v, want one", ledgers)
	}
	slots, err := l.Slots(ctx, ledgers[0])
	if err != nil || len(slots) != 2 || slots[1].CumAfter != 24000 || slots[0].Run != "refund-1041" || slots[1].Run != "refund-1049" {
		t.Fatalf("slots = %+v, %v, want the refunds of refund-1041 and refund-1049 (24000 in all)", slots, err)
	}
}
