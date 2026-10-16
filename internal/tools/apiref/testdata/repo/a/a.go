// Package a is the fixture: it shows each kind of symbol. It has a second sentence.
package a

import "errors"

// Mode is how a thing runs. Longer text follows.
type Mode int

// The modes. They are numbered.
const (
	Fast Mode = iota
	Slow
	hidden
)

// Limit is the most a thing may hold, e.g. 10 of them.
const Limit = 10

// LongText has a value too long to show: 0123456789 0123456789 0123456789 0123456789.
const LongText = "0123456789 0123456789 0123456789 0123456789 0123456789"

// ErrBad is a failure with <angle> brackets, a *star and a_snake_name; see [Mode] and {{braces}}.
var ErrBad = errors.New("bad")

// Thing is a thing — with an em dash.
type Thing struct {
	// Name is the name.
	Name string
	n    int
}

// NewThing builds a Thing.
func NewThing(name string) *Thing { return &Thing{Name: name} }

// Run runs the thing.
func (t *Thing) Run(m Mode) error { return nil }

// Pair is generic.
type Pair[K comparable, V any] struct {
	Key K
	Val V
}

// Keys lists keys.
func (p Pair[K, V]) Keys() []K { return nil }

// Doer does.
type Doer interface {
	// Do does.
	Do() error
}

// Map applies f.
func Map[T, U any](xs []T, f func(T) U) []U { return nil }

// Certish is a symbol declared in the file the rules label.
func Certish() {}
