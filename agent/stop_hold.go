package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// WithStopSwitch gives the agent a kill switch (see StopSwitch, and docs/design/kill-switch.md): its
// runs are held, as a pause (*Stopped), where a stop covers them, before any side effect that has not
// begun. An agent without one that runs on a store whose control run holds a record fails its drives
// with ErrConfig, so a process cannot silently disregard a stop. A nil switch is ErrConfig.
func WithStopSwitch(sw *StopSwitch) Option {
	return agentOption(func(c *agentConfig) error {
		if sw == nil {
			return fmt.Errorf("WithStopSwitch: nil switch: %w", ErrConfig)
		}
		c.a.stop = sw
		return nil
	})
}

type compensatingKey struct{}

// withCompensating marks ctx as the context of a saga's rollback, whose calls a stop holds only if it
// blocks compensation.
func withCompensating(ctx context.Context) context.Context {
	return context.WithValue(ctx, compensatingKey{}, true)
}

func isCompensating(ctx context.Context) bool {
	v, _ := ctx.Value(compensatingKey{}).(bool)
	return v
}

// stopHold is the kill switch's check for one place in a run: nil when nothing holds it. tool is the
// tool the place is about ("" for a drive's or a turn's start), sideEffect whether that tool can have a
// side effect, and sync whether the control run must be read now (a call about to begin).
func (a *Agent) stopHold(ctx context.Context, runID, tool string, sideEffect, sync bool) *Stopped {
	if a.stop == nil {
		return nil
	}
	id, _ := IdentityFrom(ctx)
	q := StopQuery{Tool: tool, Agent: id.Actor, Compensation: isCompensating(ctx)}
	return a.stop.Hold(ctx, RunRef{RunID: runID, RootRunID: rootRunID(ctx, runID)}, q, sideEffect, sync)
}

// stopGuardSeen keeps, by store, what controlRunGuard learned: a control run that exists never goes
// away, so that is kept for good; its absence for a few seconds.
var stopGuardSeen sync.Map // store identity -> time.Time of the last absent read, or true

const stopGuardAbsentFor = 5 * time.Second

// controlRunGuard is decision 11 of the design: a drive of an agent with no kill switch, on a store
// whose control run holds a record, fails with ErrConfig, so that no process runs agents in disregard
// of a stop. It costs one read per store per few seconds.
func (a *Agent) controlRunGuard(ctx context.Context) error {
	if a.stop != nil {
		return nil
	}
	return controlRunGuardFor(ctx, a.store)
}

// stopFlow is a plan flow's name and kill switch, carried by the context of the flow's run.
type stopFlow struct {
	name string
	sw   *StopSwitch
}

type stopFlowKey struct{}

func stopFlowFrom(ctx context.Context) *stopFlow {
	sf, _ := ctx.Value(stopFlowKey{}).(*stopFlow)
	return sf
}

// hold is the switch's check for one node of the flow: nil when nothing holds it (or the receiver is
// nil).
func (sf *stopFlow) hold(ctx context.Context, ref RunRef, sideEffect, sync bool) *Stopped {
	if sf == nil {
		return nil
	}
	id, _ := IdentityFrom(ctx)
	return sf.sw.Hold(ctx, ref, StopQuery{Flow: sf.name, Agent: id.Actor}, sideEffect, sync)
}

// controlRunGuardFor is controlRunGuard for a store with no agent: a plan flow without a switch.
func controlRunGuardFor(ctx context.Context, j *Journal) error {
	if v, ok := stopGuardSeen.Load(j.id); ok {
		if _, found := v.(bool); found {
			return errNoSwitch()
		}
		if t, isTime := v.(time.Time); isTime && time.Since(t) < stopGuardAbsentFor {
			return nil
		}
	}
	for _, err := range j.store.Load(ctx, StopControlRun, -1) {
		if err != nil {
			return storageErr("load the stop control run", err)
		}
		stopGuardSeen.Store(j.id, true)
		return errNoSwitch()
	}
	stopGuardSeen.Store(j.id, time.Now())
	return nil
}

func errNoSwitch() error {
	return fmt.Errorf("this store holds a kill switch (its control run has records) and this agent or flow has none: give the process a StopSwitch (WithStopSwitch; a flow's WithStopSwitch), or it would run in disregard of a stop: %w", ErrConfig)
}
