package plan

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/bide-ai/bide/agent"
)

// flowNotify holds a built flow's notifiers, one per store it has run against: a flow is run against
// many stores over its life, and a notifier is bound to a journal over the store it reads. Journals
// over one store (an application that wraps its store in a new Journal per request) share one.
type flowNotify struct {
	mu  sync.Mutex
	per map[any]*agent.RunNotifier // by the journal's store
}

// runEnd tells the flow's notifier, if it has one, that a drive of runID ended with err.
func (c *builderCore) runEnd(store *agent.Journal, runID string, err error) {
	if c.notifier == nil || err == nil || store == nil {
		return
	}
	key := storeKey(store)
	c.notify.mu.Lock()
	rn := c.notify.per[key]
	if rn == nil {
		var cerr error
		if rn, cerr = agent.NewRunNotifier(store, c.notifier); cerr != nil {
			c.notify.mu.Unlock()
			return // Build validated the option; a failure here has nothing to report to
		}
		if c.notify.per == nil {
			c.notify.per = map[any]*agent.RunNotifier{}
		}
		c.notify.per[key] = rn
	}
	c.notify.mu.Unlock()
	rn.DriveEnd(runID, err)
}

// storeKey identifies the store under j: the Store value when it can be a map key (a pointer, the
// usual case), else the journal itself.
func storeKey(j *agent.Journal) any {
	if st := j.Store(); st != nil && reflect.TypeOf(st).Comparable() {
		return st
	}
	return j
}

// FlushNotifications waits for the flow's notifier to finish the deliveries in flight, for every
// journal the flow has run against, as agent.Agent.FlushNotifications does. It does nothing for a
// flow with no notifier.
func (f *Flow[In, Out]) FlushNotifications(ctx context.Context) error {
	c := f.core
	if c.notifier == nil {
		return nil
	}
	c.notify.mu.Lock()
	rns := make([]*agent.RunNotifier, 0, len(c.notify.per))
	for _, rn := range c.notify.per {
		rns = append(rns, rn)
	}
	c.notify.mu.Unlock()
	var errs []error
	for _, rn := range rns {
		if err := rn.Flush(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
