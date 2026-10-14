package sample

import (
	"context"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// TestWriteNotifyTrail: the sample's trail has an acknowledged approval and abort, a failing and
// overdue halt, and a halt that is failing and not yet overdue; the same on a prefixed run.
func TestWriteNotifyTrail(t *testing.T) {
	for _, prefix := range []string{"", "p-"} {
		ctx := context.Background()
		s := agent.NewMemStore()
		if _, err := Write(ctx, s, prefix); err != nil {
			t.Fatal(err)
		}
		j, err := agent.NewJournal(s)
		if err != nil {
			t.Fatal(err)
		}
		es := audit.NewMemEventStore()
		if err := WriteNotifyTrail(ctx, j, es, prefix); err != nil {
			t.Fatal(err)
		}
		state := func(run string) (acked, failing, overdue bool, kinds string) {
			recs, err := audit.NewNotifyTrail(es).Load(ctx, agent.NotifyTrailName(prefix+run))
			if err != nil {
				t.Fatal(err)
			}
			st := agent.FoldNotify(recs)
			for _, k := range st.Keys {
				e := st.Events[k]
				if strings.HasPrefix(k, "overdue:") {
					continue
				}
				kinds += e.Kind + " "
				acked = acked || e.Acked != nil
				failing = failing || e.Failing != nil
				overdue = overdue || e.Overdue != nil
			}
			return
		}
		for run, want := range map[string][3]bool{ // acked, failing, overdue
			"refund-1043": {true, false, false},
			"triage-3004": {true, false, false},
			"ship-2001":   {true, false, false},
			"refund-1042": {false, true, true},
			"triage-3003": {false, true, false},
		} {
			a, f, o, kinds := state(run)
			if [3]bool{a, f, o} != want {
				t.Errorf("%s%s (%s): acked %v failing %v overdue %v, want %v", prefix, run, kinds, a, f, o, want)
			}
		}
	}
}
