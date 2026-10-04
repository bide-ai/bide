// Package plan is a stub of bide's pre-1.0 plan registry for the migrate tool's tests.
package plan

import "context"

type Registry struct{}

func NewRegistry() *Registry { panic("stub") }

func (r *Registry) RegisterStep[I, O any](name string, fn func(context.Context, I) (O, error)) error {
	panic("stub")
}
