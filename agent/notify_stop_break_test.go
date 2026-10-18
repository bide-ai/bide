package agent

// Reviewer's test for #414 M1 (copy into package agent as an _test.go file). Uses the repo helpers
// newControl, putRaw, mustState, stopSweep, fakeSink, sweepClock, NewMemNotifyTrail.

import (
	"testing"
	"time"
)

// A stop whose writer-asserted time is old (a backdated or clock-skewed At) is never notified: the window
// is judged on a time the writer chose.
func TestBreak_BackdatedStopIsNeverNotified(t *testing.T) {
	ctl := newControl(t, NewMemStore())
	r := StopRecord{Format: StopFormat, Kind: "set", Mode: StopBasic, Scope: &StopScope{Kind: StopGlobal}, Compensation: CompensationAllow, Reason: "quiet stop", By: "ops", At: "2026-01-01T00:00:00Z"}
	r.StopID = StopIDOf(r)
	putRaw(t, ctl, r)
	if n := len(mustState(t, ctl).Active()); n != 1 {
		t.Fatalf("the stop is not in force: %d", n)
	}
	trail, sink, clk := NewMemNotifyTrail(), &fakeSink{}, sweepClock()
	clk.advance(time.Hour)
	if err := stopSweep(t, ctl, sink, trail, clk); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 {
		t.Errorf("a stop in force that was recorded just now (At backdated) raised %d notifications", sink.count())
	}
}
