package plan

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	amodel "github.com/bide-ai/bide/plan/internal/digesttypes/a/model"
	bmodel "github.com/bide-ai/bide/plan/internal/digesttypes/b/model"
)

// namesRegistry registers two predicates over the same type, and the "approve" node's block
// under the given name with the same types as decline, so a config can swap a predicate or a
// block without any type changing.
func namesRegistry(t *testing.T, block string) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, err := range []error{
		RegisterStep(reg, "classify", cfgClassify),
		RegisterStep(reg, block, func(_ context.Context, a cfgAssessment) (cfgReceipt, error) {
			return cfgReceipt{ID: a.ID, Status: block}, nil
		}),
		RegisterStep(reg, "decline", cfgDecline),
		RegisterPredicate(reg, "rush", func(a cfgAssessment) bool { return a.Rush }),
		RegisterPredicate(reg, "notRush", func(a cfgAssessment) bool { return !a.Rush }),
	} {
		if err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	return reg
}

// namesConfig is a switch over classify with one predicate arm and an else. pred and block
// choose which registered predicate the arm uses and which block the "approve" node runs.
func namesConfig(pred, block string) string {
	return `{"version":1,"flow":"names","nodes":[
    {"name":"classify","block":"classify"},
    {"name":"approve","block":"` + block + `"},
    {"name":"decline","block":"decline"}],
  "wiring":[{"switch":"classify","when":[{"pred":"` + pred + `","to":"approve"}],"else":"decline"}]}`
}

func loadNames(t *testing.T, pred, block string) *Flow[cfgOrder, cfgReceipt] {
	t.Helper()
	f, err := Load[cfgOrder, cfgReceipt]([]byte(namesConfig(pred, block)), namesRegistry(t, block))
	if err != nil {
		t.Fatalf("Load(%s, %s): %v", pred, block, err)
	}
	return f
}

// The digest commits to which registered predicate an arm tests and which registered block a
// node runs: a config that swaps either one describes a different flow, so it must have a
// different digest.
func TestDigest_CommitsToPredicateAndBlockNames(t *testing.T) {
	base := loadNames(t, "rush", "approve").Digest()
	if d := loadNames(t, "notRush", "approve").Digest(); d == base {
		t.Errorf("swapping predicate rush for notRush kept digest %s", d)
	}
	if d := loadNames(t, "rush", "hold").Digest(); d == base {
		t.Errorf("swapping block approve for hold (same types) kept digest %s", d)
	}
}

// A run started under one flow is not resumed under a flow that tests a different predicate:
// the resume guard compares digests, so the digest must tell the two apart.
func TestRun_RefusesAFlowWithASwappedPredicate(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	j := agenttest.MustJournal(store)
	// The first drive does not complete (its completion is lost), so the run is resumed, not
	// returned as finished.
	if _, err := loadNames(t, "rush", "approve").Run(ctx, agenttest.MustJournal(noComplete{store}), "r1", cfgOrder{ID: 1, Rush: true}); !errors.Is(err, errNoComplete) {
		t.Fatalf("first run: %v", err)
	}
	_, err := loadNames(t, "notRush", "approve").Run(ctx, j, "r1", cfgOrder{ID: 1, Rush: true})
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("resume under a flow with a swapped predicate: err = %v, want ErrConfig", err)
	}
}

// Two types from different packages share reflect.Type.String() ("model.Req"). A flow whose
// step takes one is a different flow from one whose step takes the other.
func TestDigest_TellsTypesApartByPackagePath(t *testing.T) {
	if (reflectName[amodel.Req]()) != (reflectName[bmodel.Req]()) {
		t.Fatalf("fixture: the two types should print alike")
	}
	fa := New[amodel.Req, amodel.Req]("f")
	fa.Step("s", func(_ context.Context, r amodel.Req) (amodel.Req, error) { return r, nil })
	a, err := fa.Build()
	if err != nil {
		t.Fatal(err)
	}
	fb := New[bmodel.Req, bmodel.Req]("f")
	fb.Step("s", func(_ context.Context, r bmodel.Req) (bmodel.Req, error) { return r, nil })
	b, err := fb.Build()
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() == b.Digest() {
		t.Errorf("flows over a/model.Req and b/model.Req share digest %s", a.Digest())
	}
}

func reflectName[T any]() string {
	return strings.TrimSpace(typeName(reflect.TypeFor[T]()))
}
