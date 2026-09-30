// Package good is documented everywhere the policy asks, and shows what it does not ask.
package good

import "encoding/json"

// Kind is a closed set.
type Kind string

// The kinds, documented by their group.
const (
	KindA Kind = "a"
	KindB Kind = "b" // a line comment is not required
)

const (
	// KindC has its own comment.
	KindC Kind = "c"
)

// Max is a single const.
const Max = 3

// Default, Fallback are documented by either name.
var Default, Fallback = 1, 2

// Fallback2 or Default2: the comment may start with any name in the spec.
var Default2, Fallback2 = 1, 2

var (
	// Registry has a comment in a group without one.
	Registry   = map[string]int{}
	unexported = 1
)

var _ json.Marshaler = T{}

// A T opens with an article, which a type may do.
type T struct {
	Field int // fields are not checked
}

// An Opt opens with "An".
type Opt func(*T)

// The Set opens with "The".
type Set[E comparable] map[E]struct{}

type (
	// Grouped has its own comment in a group.
	Grouped int
)

// Error opens with the method name.
func (T) Error() string { return "" }

// Unwrap opens with the method name.
func (T) Unwrap() error { return nil }

// MarshalJSON opens with the method name.
func (T) MarshalJSON() ([]byte, error) { return nil, nil }

// UnmarshalJSON opens with the method name.
func (*T) UnmarshalJSON([]byte) error { return nil }

// String opens with the method name.
func (T) String() string { return "" }

// Add is a method on a generic receiver.
func (s Set[E]) Add(e E) { s[e] = struct{}{} }

// New is a func. Its comment may end with a deprecation.
//
// Deprecated: use T.
func New() T { return T{} }

// Keep's name is followed by punctuation, which ends the name.
func Keep() {}

// unexported identifiers and methods on unexported types need no comment.
type hidden struct{}

func (hidden) Exported() {}

func helper() {}

func (T) unexportedMethod() {}

// Iface's methods are part of its type, not separate declarations.
type Iface interface {
	Method()
}
