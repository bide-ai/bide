package agent_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// imitation embeds RunRef and is an error with Paused, the whole exported surface of a Pause. The
// seal must still keep it out: code outside the package cannot add a pause kind the engine does
// not know how to hold, propagate, or resume.
type imitation struct{ agent.RunRef }

func (*imitation) Error() string { return "imitation pause" }

var pauseType = reflect.TypeFor[agent.Pause]()

func TestPause_Sealed(t *testing.T) {
	if reflect.TypeFor[*imitation]().Implements(pauseType) {
		t.Fatal("a type outside the package that embeds RunRef satisfies Pause: the seal is broken")
	}
	err := fmt.Errorf("wrapped: %w", &imitation{agent.RunRef{RunID: "r1"}})
	if agent.IsPause(err) {
		t.Fatal("IsPause accepted an imitation")
	}
	if p, ok := agent.AsPause(err); ok || p != nil {
		t.Fatalf("AsPause accepted an imitation: %v", p)
	}
}

// The five pause kinds, and only through a pointer, as the engine returns them.
func TestPause_Kinds(t *testing.T) {
	ref := agent.RunRef{RunID: "r1>c1", RootRunID: "r1"}
	for _, p := range []agent.Pause{
		&agent.ApprovalPending{RunRef: ref},
		&agent.InterruptPending{RunRef: ref},
		&agent.SignalPending{RunRef: ref},
		&agent.TimerPending{RunRef: ref},
		&agent.OutcomeUnknown{RunRef: ref},
	} {
		if p.Paused() != ref {
			t.Errorf("%T.Paused() = %+v, want %+v", p, p.Paused(), ref)
		}
		got, ok := agent.AsPause(fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", p)))
		if !ok || got != p || !agent.IsPause(got) {
			t.Errorf("AsPause did not find a wrapped %T", p)
		}
		if reflect.TypeOf(p).Elem().Implements(pauseType) {
			t.Errorf("%v (not a pointer) satisfies Pause", reflect.TypeOf(p).Elem())
		}
	}
	if agent.IsPause(nil) || agent.IsPause(agent.ErrStorage) {
		t.Fatal("IsPause accepted a non-pause")
	}
}

// The former names are aliases of the same types, so code matching either keeps working until
// the 1.0 rewrite removes them.
func TestPause_TransitionalAliases(t *testing.T) {
	for _, c := range [][2]reflect.Type{
		{reflect.TypeFor[agent.ApprovalPending](), reflect.TypeFor[agent.ApprovalPending]()},
		{reflect.TypeFor[agent.InterruptPending](), reflect.TypeFor[agent.InterruptPending]()},
		{reflect.TypeFor[agent.SignalPending](), reflect.TypeFor[agent.SignalPending]()},
		{reflect.TypeFor[agent.TimerPending](), reflect.TypeFor[agent.TimerPending]()},
		{reflect.TypeFor[agent.OutcomeUnknown](), reflect.TypeFor[agent.OutcomeUnknown]()},
	} {
		if c[0] != c[1] {
			t.Errorf("%v is not an alias of %v", c[0], c[1])
		}
	}
}
