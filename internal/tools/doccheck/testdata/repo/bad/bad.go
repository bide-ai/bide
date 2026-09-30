// Package bad has one finding per marked line.
package bad

type Kind string // want `bad Kind: exported type Kind has no doc comment`

const (
	KindA Kind = "a" // want `bad KindA: exported const KindA has no doc comment`
	// KindB is fine.
	KindB Kind = "b"
	// Wrong comment.
	KindC Kind = "c" // want `bad KindC: doc comment of exported const KindC should start with "KindC"`
)

const Max = 3 // want `bad Max: exported const Max has no doc comment`

// Maximum is not Min.
const Min = 1 // want `bad Min: doc comment of exported const Min should start with "Min"`

var Registry = map[string]int{} // want `bad Registry: exported var Registry has no doc comment`

var A, b, C = 1, 2, 3 // want `bad A: exported var A` `bad C: exported var C`

var (
	// Starts with neither exported name.
	D, E = 1, 2 // want `bad D: doc comment of exported var D should start with "D"` `bad E: doc comment of exported var E should start with "D"`
)

type T struct{} // want `bad T: exported type T has no doc comment`

// Tee is not T, and T followed by a letter is another word.
type Te int // want `bad Te: doc comment of exported type Te should start with "Te"`

// A Wrong names another type after its article.
type Right int // want `bad Right: doc comment of exported type Right should start with "Right"`

// Deprecated: a deprecation alone does not name the identifier.
type Old int // want `bad Old: doc comment of exported type Old should start with "Old"`

// The group comment does not cover a type in a group.
type (
	Grouped int // want `bad Grouped: exported type Grouped has no doc comment`
)

//go:noinline
func Directive() {} // want `bad Directive: exported func Directive has no doc comment`

func New() T { return T{} } // want `bad New: exported func New has no doc comment`

// A func may not open with an article.
func Article() {} // want `bad Article: doc comment of exported func Article should start with "Article"`

// Newer is a different name.
func NewX() {} // want `bad NewX: doc comment of exported func NewX should start with "NewX"`

func (T) Method() {} // want `bad T.Method: exported method T.Method has no doc comment`

// T.Qualified is not how a method comment starts.
func (*T) Qualified() {} // want `bad T.Qualified: doc comment of exported method T.Qualified should start with "Qualified"`

type Set[E comparable] map[E]struct{} // want `bad Set: exported type Set`

func (s Set[E]) Add(e E) {} // want `bad Set.Add: exported method Set.Add has no doc comment`

type Pair[K comparable, V any] struct{} // want `bad Pair: exported type Pair`

func (*Pair[K, V]) Get() {} // want `bad Pair.Get: exported method Pair.Get has no doc comment`
