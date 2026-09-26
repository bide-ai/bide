package plan

// Producer[M] is the output side of a wired step: it yields a value of type M.
// Handle[I,O] satisfies Producer[O]. The marker method is unexported so only
// handles (and other internal types in this package) can be producers; it
// carries no runtime behavior.
type Producer[M any] interface{ producerMarker(M) }

// Consumer[M] is the input side of a wired step: it accepts a value of type M.
// Handle[I,O] satisfies Consumer[I]. The marker method is unexported (a phantom
// method) so only handles can be consumers; it carries no runtime behavior.
type Consumer[M any] interface{ consumerMarker(M) }

// Handle[I,O] is a compile-time name for a durable step consuming I and
// producing O. It satisfies Producer[O] and Consumer[I], so wiring calls unify
// the connecting type at compile time and only handles can be endpoints. It
// carries the step's journal key (name) and a back-reference to the owning
// builder's spec; the value it stands for lives in the journal at run time.
type Handle[I, O any] struct {
	name string
	b    *builderCore // back-reference to the owning builder's spec (internal)
}

func (Handle[I, O]) producerMarker(O) {}
func (Handle[I, O]) consumerMarker(I) {}

// Name returns the durable journal-step key this handle names.
func (h Handle[I, O]) Name() string { return h.name }
