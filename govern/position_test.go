package govern_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern"
)

// badPositionLog is an EventLog whose Append records the event but reports pos instead of its
// position, as a buggy adapter or a tampered store might.
type badPositionLog struct {
	*govern.MemEventLog
	pos int64
}

func (l *badPositionLog) Append(ctx context.Context, entity, id, event string) (int64, error) {
	if _, err := l.MemEventLog.Append(ctx, entity, id, event); err != nil {
		return 0, err
	}
	return l.pos, nil
}

// A position is at least 0: Append reports how many events came before this one. A log that
// reports a negative position is broken, and a governor refuses it with ErrProtocol instead of
// slicing its events with it (a panic below -1) or reading -1 as "no event" and reporting the
// initial state as the state after the append.
func TestApply_RefusesANegativePosition(t *testing.T) {
	ctx := context.Background()
	for _, pos := range []int64{-1, -3} {
		m := buildTwoCounters(t)
		log := &badPositionLog{MemEventLog: govern.NewMemEventLog(), pos: 0}
		pg, err := govern.NewPersistent(ctx, m, log, "e", m.NewState())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pg.Apply(ctx, "inc_a"); err != nil {
			t.Fatal(err)
		}
		log.pos = pos
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("persistent, position %d: Apply panicked: %v", pos, r)
				}
			}()
			a, err := pg.Apply(ctx, "inc_b")
			if !errors.Is(err, agent.ErrProtocol) {
				t.Errorf("persistent, position %d: Apply = %+v, %v; want ErrProtocol", pos, a, err)
			}
		}()

		fm, _, _, _, _ := buildMfrSupFederation(t)
		flog := &badPositionLog{MemEventLog: govern.NewMemEventLog(), pos: 0}
		fg, err := govern.NewFederated(ctx, fm, flog, "f", fm.NewState())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fg.Apply(ctx, "manufacturer", "epub"); err != nil {
			t.Fatal(err)
		}
		flog.pos = pos
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("federated, position %d: Apply panicked: %v", pos, r)
				}
			}()
			a, err := fg.Apply(ctx, "supplier", "eexp")
			if !errors.Is(err, agent.ErrProtocol) {
				t.Errorf("federated, position %d: Apply = %+v, %v; want ErrProtocol", pos, a, err)
			}
		}()
	}
}
