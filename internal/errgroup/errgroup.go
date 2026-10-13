// Package errgroup runs a group of functions in goroutines, collects the first error, and cancels
// the group's context when one fails. It is the subset of golang.org/x/sync/errgroup the agent loop
// uses (WithContext, SetLimit, Go, Wait), with the same semantics, written against the standard
// library so the core module requires nothing outside it.
package errgroup

import (
	"context"
	"fmt"
	"sync"
)

// A Group is a set of goroutines working on one task. Create it with WithContext.
type Group struct {
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
	sem    chan struct{} // nil: no limit

	errOnce sync.Once
	err     error
}

// WithContext returns a new Group and a context derived from ctx. The context is cancelled, with
// the error as its cause, the first time a function passed to Go returns a non-nil error, and
// otherwise when Wait returns.
func WithContext(ctx context.Context) (*Group, context.Context) {
	ctx, cancel := context.WithCancelCause(ctx)
	return &Group{cancel: cancel}, ctx
}

// SetLimit limits the number of functions running at once to n; a negative n removes the limit.
// It panics if functions are running under a limit, as x/sync/errgroup's does: they would release
// their slots into the new semaphore and block. Call it before the first Go.
func (g *Group) SetLimit(n int) {
	if n < 0 {
		g.sem = nil
		return
	}
	if active := len(g.sem); active != 0 {
		panic(fmt.Errorf("errgroup: modify limit while %v goroutines in the group are still active", active))
	}
	g.sem = make(chan struct{}, n)
}

// Go runs f in a new goroutine, first blocking while the group is at its limit. The first non-nil
// error any f returns is Wait's result and cancels the group's context.
func (g *Group) Go(f func() error) {
	if g.sem != nil {
		g.sem <- struct{}{}
	}
	g.wg.Add(1)
	go func() {
		defer func() {
			if g.sem != nil {
				<-g.sem
			}
			g.wg.Done()
		}()
		if err := f(); err != nil {
			g.errOnce.Do(func() {
				g.err = err
				g.cancel(err)
			})
		}
	}()
}

// Wait blocks until every function passed to Go has returned, cancels the group's context, and
// returns the first non-nil error, if any.
func (g *Group) Wait() error {
	g.wg.Wait()
	g.cancel(g.err)
	return g.err
}
