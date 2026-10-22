package approvecmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// -list does not read a cap ledger run: the ledger holds slots, no run and no gate. A record in it
// that is not a journal record (nothing here writes one, and a store may hold anything) is not
// an error, and the waiting gate of the ordinary run is still listed.
func TestList_SkipsCapLedgerRuns(t *testing.T) {
	f := newFixture(t, twoOfThree, refundArgs)
	j, closeStore := f.store()
	for _, name := range []string{"slot:0", "approval-pending:c9"} {
		if _, _, err := j.Store().Insert(context.Background(), "@cap/ns/refunds/usd/d2026-10-22/0123abcd", name, []byte("not a journal record")); err != nil {
			t.Fatal(err)
		}
	}
	closeStore()
	code, out, errs := f.run_("", "-list", "-json")
	if code != 0 {
		t.Fatalf("-list over a store with a ledger run: exit %d: %s", code, errs)
	}
	var got []waiting
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 || got[0].Run != f.run {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Contains(out, "@cap/") {
		t.Fatalf("the ledger run is in the list: %s", out)
	}
}
