// Package plan is a stub of bide v0.10's plan registry for the migrate tool's tests.
package plan

import "context"

type Registry struct{}

func NewRegistry() *Registry { panic("stub") }

func RegisterStep[I, O any](r *Registry, name string, fn func(context.Context, I) (O, error)) error {
	panic("stub")
}
