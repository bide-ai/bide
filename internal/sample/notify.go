package sample

import (
	"context"
	"fmt"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// WriteNotifyTrail runs the real notifier's catch-up sweep (agent.NotifyPending) twice over the
// sample's journal, with a scripted sink and a fixed clock, and so writes the notification trail
// the Needs attention list reads, into es. The first sweep, at 10:10 UTC on the sample's first day,
// is acknowledged for every event it sees except the halted refund-1042, whose sink refuses it:
// that delivery is failing. The second, at 10:40, finds the 10:10 refund-1042 event past its
// 5-minute deadline (overdue) and sees the halted flow node of triage-3003 for the first time,
// whose sink refuses too (failing, not yet overdue). prefix is the one given to Write.
//
// What it records is deterministic but for the random salt each trail record carries.
func WriteNotifyTrail(ctx context.Context, j *agent.Journal, es audit.EventStore, prefix string) error {
	trail := audit.NewNotifyTrail(es)
	var now time.Time
	sink := refuse{prefix + "refund-1042": true, prefix + "triage-3003": true}
	opts := []agent.NotifyOption{
		agent.WithNotifyClock(func() time.Time { return now }),
		agent.WithNotifyDeadline(5 * time.Minute),
		agent.WithNotifyFailingAfter(1),
		agent.WithNotifyHaltAge(5 * time.Minute),
		agent.WithNotifyHolder("sample-notifier"),
	}
	for _, at := range []time.Time{Base.Add(70 * time.Minute), Base.Add(100 * time.Minute)} {
		now = at
		// A refused delivery is the sample's point: the sweep reports it, and the trail records it.
		_ = agent.NotifyPending(ctx, j, sink, trail, opts...)
	}
	// What the sweeps could not write is not an error they return alone: check the trail itself.
	recs, err := trail.Load(ctx, agent.NotifyTrailName(prefix+"refund-1043"))
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return fmt.Errorf("sample: the notification sweep wrote no trail for %srefund-1043", prefix)
	}
	return nil
}

// refuse is a Notifier whose sink refuses, for the runs it names, with a 4xx that a retry within the round cannot fix.
type refuse map[string]bool

func (r refuse) Notify(ctx context.Context, n agent.Notification) error {
	if r[n.RunID] {
		return fmt.Errorf("sink: 403: %w", agent.ErrNotifyPermanent) // no retry within the round: the sample stays fast
	}
	agent.ReportNotifyAck(ctx, agent.NotifyAck{Status: 202})
	return nil
}
