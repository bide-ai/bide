package plan

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// stubTool is a minimal agent.Tool used to exercise Builder.Tool without a real
// provider. Call echoes its args back so the JSON round-trip can be asserted.
type stubTool struct {
	safety agent.Safety
	last   json.RawMessage
}

func (t *stubTool) Name() string                { return "stub" }
func (t *stubTool) Description() string         { return "echo tool for tests" }
func (t *stubTool) ArgsSchema() json.RawMessage { return nil }
func (t *stubTool) Safety() agent.Safety        { return t.safety }
func (t *stubTool) Call(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	t.last = args
	return args, nil
}

// TestBuilderConstructsFlow builds a small flow (Step -> Tool -> Switch(When/Else))
// and asserts the reified spec records the right nodes, entry, edges, and branch.
func TestBuilderConstructsFlow(t *testing.T) {
	b := New[int, string]("small")

	start := b.Step("start", func(n int) (int, error) { return n + 1, nil })
	echo := b.Tool[int, int]("echo", &stubTool{})
	yes := b.Step("yes", func(int) (string, error) { return "yes", nil })
	no := b.Step("no", func(int) (string, error) { return "no", nil })

	b.Edge(start, echo)
	b.Switch(echo,
		When(func(n int) bool { return n > 0 }, yes),
		Else(no),
	)

	core := b.core

	if core.flowName != "small" {
		t.Errorf("flowName = %q, want %q", core.flowName, "small")
	}
	if core.entry != "start" {
		t.Errorf("entry = %q, want %q (first constructed node)", core.entry, "start")
	}
	if core.inType != reflect.TypeFor[int]() || core.outType != reflect.TypeFor[string]() {
		t.Errorf("boundary types = (%s, %s), want (int, string)", core.inType, core.outType)
	}
	if len(core.nodes) != 4 {
		t.Fatalf("nodes = %d, want 4", len(core.nodes))
	}
	if len(core.errs) != 0 {
		t.Fatalf("unexpected construction errors: %v", core.errs)
	}

	// Handles carry the right names.
	if start.Name() != "start" || echo.Name() != "echo" || yes.Name() != "yes" || no.Name() != "no" {
		t.Errorf("handle names wrong: %q %q %q %q", start.Name(), echo.Name(), yes.Name(), no.Name())
	}

	// Edge records names only.
	if len(core.edges) != 1 || core.edges[0] != (edge{from: "start", to: "echo"}) {
		t.Errorf("edges = %+v, want one start->echo", core.edges)
	}

	// Switch records one branch over "echo" with two arms, exactly one Else.
	if len(core.branches) != 1 {
		t.Fatalf("branches = %d, want 1", len(core.branches))
	}
	br := core.branches[0]
	if br.over != "echo" {
		t.Errorf("branch over = %q, want echo", br.over)
	}
	if len(br.arms) != 2 {
		t.Fatalf("arms = %d, want 2", len(br.arms))
	}
	if br.arms[0].isElse || br.arms[0].target != "yes" || br.arms[0].pred == nil {
		t.Errorf("arm0 = %+v, want When->yes with predicate", br.arms[0])
	}
	if !br.arms[1].isElse || br.arms[1].target != "no" || br.arms[1].pred != nil {
		t.Errorf("arm1 = %+v, want Else->no with nil predicate", br.arms[1])
	}
}

// TestBuilderNodeKindsAndTypes asserts each constructor records its kind and I/O
// reflect types. A Step/Tool installs a run closure; a Model node installs no run
// closure (runNode dispatches a kindModel node to runModel) and records its prompt.
func TestBuilderNodeKindsAndTypes(t *testing.T) {
	b := New[int, int]("kinds")
	b.Step("s", func(int) (bool, error) { return true, nil })
	b.Tool[string, int]("t", &stubTool{})
	b.Model[int, string]("m", "prompt text")

	want := []struct {
		name    string
		kind    nodeKind
		in, out reflect.Type
	}{
		{"s", kindStep, reflect.TypeFor[int](), reflect.TypeFor[bool]()},
		{"t", kindTool, reflect.TypeFor[string](), reflect.TypeFor[int]()},
		{"m", kindModel, reflect.TypeFor[int](), reflect.TypeFor[string]()},
	}
	for i, w := range want {
		n := b.core.nodes[i]
		if n.name != w.name || n.kind != w.kind || n.inType != w.in || n.outType != w.out {
			t.Errorf("node %d = {%q %v %s %s}, want {%q %v %s %s}",
				i, n.name, n.kind, n.inType, n.outType, w.name, w.kind, w.in, w.out)
		}
		if w.kind == kindModel {
			if n.run != nil {
				t.Errorf("node %q (Model) has a run closure; want nil so runNode dispatches to runModel", n.name)
			}
			if n.prompt != "prompt text" {
				t.Errorf("node %q recorded prompt %q, want %q", n.name, n.prompt, "prompt text")
			}
			continue
		}
		if n.run == nil {
			t.Errorf("node %q run closure is nil", n.name)
		}
	}
}

// TestBuilderDuplicateNameRecordsError asserts a duplicate step name appends a
// deferred error to core.errs (surfaced at Build) rather than panicking, and does
// not overwrite the first node.
func TestBuilderDuplicateNameRecordsError(t *testing.T) {
	b := New[int, int]("dup")
	b.Step("same", func(int) (int, error) { return 0, nil })
	b.Step("same", func(int) (int, error) { return 1, nil })

	if len(b.core.errs) != 1 {
		t.Fatalf("errs = %d, want 1 duplicate error; errs=%v", len(b.core.errs), b.core.errs)
	}
	if len(b.core.nodes) != 1 {
		t.Errorf("nodes = %d, want 1 (duplicate not registered)", len(b.core.nodes))
	}
	if b.core.byName["same"] == nil {
		t.Error("byName lost the original node")
	}
}

// TestStepRunClosure exercises the type-erased Step run closure: it decodes the
// input as I, calls fn, and boxes the result.
func TestStepRunClosure(t *testing.T) {
	b := New[int, int]("run")
	b.Step("double", func(n int) (int, error) { return n * 2, nil })
	got, err := b.core.nodes[0].run(context.Background(), 21)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != 42 {
		t.Errorf("run(21) = %v, want 42", got)
	}
}

// TestToolRunClosureRoundTrips exercises the Tool run closure: it JSON-encodes the
// input, calls the tool, and JSON-decodes the result to O.
func TestToolRunClosureRoundTrips(t *testing.T) {
	b := New[int, int]("tool")
	tool := &stubTool{safety: agent.Safety{ReadOnly: true}}
	b.Tool[int, int]("echo", tool)
	got, err := b.core.nodes[0].run(context.Background(), 7)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != 7 {
		t.Errorf("tool run(7) = %v, want 7", got)
	}
	if string(tool.last) != "7" {
		t.Errorf("tool received args %q, want 7", tool.last)
	}
}

// TestModelLoweringRecordsPromptNoStub asserts the Model lowering records the prompt
// template and installs no run closure (runNode dispatches a kindModel node to
// runModel, which reads the bound model at run time). The rung-1 stub error is gone.
func TestModelLoweringRecordsPromptNoStub(t *testing.T) {
	b := New[int, string]("model")
	b.Model[int, string]("m", "prompt {{.}}")
	n := b.core.nodes[0]
	if n.kind != kindModel {
		t.Errorf("node kind = %v, want kindModel", n.kind)
	}
	if n.run != nil {
		t.Error("Model node has a run closure; want nil so runNode dispatches to runModel")
	}
	if n.prompt != "prompt {{.}}" {
		t.Errorf("recorded prompt = %q, want %q", n.prompt, "prompt {{.}}")
	}
}

// TestWiringPredicateErasure asserts Switch type-erases the typed predicate into
// func(any)bool while preserving its behavior, and that a wrong dynamic type does
// not match (returns false rather than panicking).
func TestWiringPredicateErasure(t *testing.T) {
	b := New[int, string]("pred")
	over := b.Step("over", func(int) (int, error) { return 0, nil })
	target := b.Step("target", func(int) (string, error) { return "", nil })
	b.Switch(over, When(func(n int) bool { return n > 5 }, target))

	pred := b.core.branches[0].arms[0].pred
	if !pred(10) {
		t.Error("erased pred(10) = false, want true")
	}
	if pred(3) {
		t.Error("erased pred(3) = true, want false")
	}
	if pred("not an int") {
		t.Error("erased pred on wrong dynamic type = true, want false")
	}
}

// Type-unification is a compile-time property of Edge/When/Else/Switch. The
// positive wiring in the tests above (start:Handle[int,int] -> echo:Handle[int,int])
// compiles precisely because M unifies both endpoints' connecting type.
//
// The following negative case is documented rather than executed: it must NOT
// compile, so it lives in a comment. Verified by the author with `go build` after
// uncommenting locally:
//
//	b := New[int, string]("neg")
//	a := b.Step("a", func(int) (int, error) { return 0, nil })    // Producer[int]
//	c := b.Step("c", func(string) (string, error) { return "", nil }) // Consumer[string]
//	b.Edge(a, c) // compile error: int (a's Out) != string (c's In); M cannot unify
//
// Edge's signature Edge[M any](from Producer[M], to Consumer[M]) forces from.Out
// and to.In to the same M, so the mismatch is rejected at the call site naming the
// handles.
