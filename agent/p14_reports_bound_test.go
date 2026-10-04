package agent

import "testing"

// The reports a recovery pass makes once per process (ErrNotStarted, ErrNotResumable) are held in
// a bounded set: past its bound, the least recently reported run is forgotten (and is reported
// again if a later pass finds it so), so a process whose passes meet many unstarted runs does not
// grow without bound.
func TestP14_RecoverReportsAreBounded(t *testing.T) {
	old := maxRecoverReports
	maxRecoverReports = 2
	t.Cleanup(func() { maxRecoverReports = old })
	store := memJournal()
	for _, id := range []string{"bound-a", "bound-b", "bound-c"} {
		if !reportOnce(store, id, ErrNotStarted) {
			t.Fatalf("the first report of %s was suppressed", id)
		}
	}
	if n := recoverReportCount(); n > 2 {
		t.Fatalf("the report set holds %d entries, bound 2", n)
	}
	if reportOnce(store, "bound-c", ErrNotStarted) {
		t.Fatal("a run reported most recently was reported again")
	}
	if !reportOnce(store, "bound-a", ErrNotStarted) {
		t.Fatal("the least recently reported run, past the bound, was still remembered")
	}
}
