package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// keyConstructors maps every function and constant named *Step that builds a journal key to a
// function that builds one from a sample string. TestEngineKeys_ConstructorsAreListed keeps
// the map complete, so a new key constructor is checked by the tests below.
var keyConstructors = map[string]func(string) string{
	"runCompleteStep":      func(string) string { return runCompleteStep },
	"runAbortedStep":       func(string) string { return runAbortedStep },
	"runStartStep":         func(string) string { return runStartStep },
	"runCancelledStep":     func(string) string { return runCancelledStep },
	"runLimitsStep":        func(s string) string { return runLimitsStep(len(s)) },
	"headerStep":           func(string) string { return headerStep },
	"modelStep":            func(s string) string { return modelStep(len(s)) },
	"ToolResultStep":       ToolResultStep,
	"toolAttemptStep":      toolAttemptStep,
	"stepAttemptStep":      stepAttemptStep,
	"retryAttemptStep":     func(s string) string { return retryAttemptStep(toolAttemptStep(s), 1+len(s)) },
	"notStartedStep":       func(s string) string { return notStartedStep(toolAttemptStep(s), "0123abcd") },
	"nextAttemptStep":      func(s string) string { return nextAttemptStep(retryAttemptStep(stepAttemptStep(s), 1+len(s))) },
	"approvalStep":         approvalStep,
	"approvalDecisionStep": func(s string) string { return approvalDecisionStep(s, "ops:1", true, []byte(s)) },
	"ApprovalTallyStep":    ApprovalTallyStep,
	"sagaCompensateStep":   sagaCompensateStep,
	"sagaArgsStep":         sagaArgsStep,
	"signalStep":           signalStep,
	"awaitTimeoutStep":     awaitTimeoutStep,
	"awaitResolvedStep":    awaitResolvedStep,
	"timerStep":            timerStep,
	"interruptStep":        interruptStep,
	"chanStep":             func(s string) string { return chanStep(s, s) },
	"chanAckStep":          func(s string) string { return chanAckStep(s, s) },
	"sessionTurnStep":      func(s string) string { return sessionTurnStep(len(s)) },
	"sessionStartStep":     func(s string) string { return sessionStartStep(len(s)) },
	"sessionFromStep":      sessionFromStep,
	"retrievalStep":        func(s string) string { return retrievalStep(len(s)) },
	"spendStep":            func(s string) string { return spendStep(len(s)) },
	"planScopedStep": func(s string) string {
		return planScopedStep(context.WithValue(context.Background(), planScopeKey{}, planScope{runID: "r", node: "node:n"}), "r", s)
	},
}

// Every key the engine builds starts with a prefix a developer-chosen step name may not use.
func TestEngineKeys_StartWithAReservedPrefix(t *testing.T) {
	for name, build := range keyConstructors {
		for _, s := range adversarialToolUseIDs() {
			if k := build(s); !IsReservedStepName(k) {
				t.Fatalf("%s(%.30q) = %.60q, which no reserved prefix covers", name, s, k)
			}
		}
	}
}

// No two engine keys built from different constructors or different inputs are equal, and no
// two sub-run IDs are, whatever the tool-use IDs.
func TestEngineKeys_AreDistinct(t *testing.T) {
	seen := map[string]string{}
	add := func(k, from string) {
		if prev, dup := seen[k]; dup && prev != from {
			t.Fatalf("key %.60q is built by both %s and %s", k, prev, from)
		}
		seen[k] = from
	}
	for name, build := range keyConstructors {
		for _, s := range adversarialToolUseIDs() {
			from := name + "(" + s + ")"
			if name == "runCompleteStep" || name == "runAbortedStep" || name == "runStartStep" || name == "runCancelledStep" || name == "headerStep" {
				from = name // a constant
			}
			if name == "sessionTurnStep" || name == "sessionStartStep" || name == "modelStep" || name == "retrievalStep" || name == "spendStep" || name == "runLimitsStep" {
				from = name + "(" + build(s) + ")" // takes a number: equal numbers give equal keys
			}
			add(build(s), from)
		}
	}
	// A digest-form ID and an ID spelled like that digest stay apart.
	sum := sha256.Sum256([]byte(strings.Repeat("k", 5000)))
	for _, id := range []string{hex.EncodeToString(sum[:]), "~" + hex.EncodeToString(sum[:])} {
		add(ToolResultStep(id), "ToolResultStep("+id+")")
	}
	runs := map[string]string{}
	for _, parent := range []string{"r", "r/1", SubRunID("r", "a"), SubRunID("r", "a>b")} {
		for _, id := range adversarialToolUseIDs() {
			k := SubRunID(parent, id)
			if prev, dup := runs[k]; dup {
				t.Fatalf("sub-run %.60q is both %s and (%s, %.30q)", k, prev, parent, id)
			}
			runs[k] = parent + " " + id
		}
	}
}

// An encoded tool-use ID is short and uses only characters no key or run ID is built with.
func TestEncodeID_IsBoundedAndPlain(t *testing.T) {
	for _, id := range adversarialToolUseIDs() {
		e := encodeID(id)
		if len(e) > maxEncodedID {
			t.Fatalf("encodeID(%.30q) is %d bytes, over %d", id, len(e), maxEncodedID)
		}
		for i := 0; i < len(e); i++ {
			c := e[i]
			ok := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("._-%", c) >= 0 || c == '~' && i == 0
			if !ok {
				t.Fatalf("encodeID(%.30q) = %q has %q", id, e, c)
			}
		}
	}
	for _, id := range []string{"toolu_01A09q90qw90lq917835lq9", "call_abc-12.3"} {
		if got := encodeID(id); got != id {
			t.Fatalf("a provider's plain ID %q was changed to %q", id, got)
		}
	}
}

// Every key built from a tool-use ID is short, printable ASCII, whatever the ID.
func TestToolUseIDKeys_AreBoundedAndPrintable(t *testing.T) {
	for _, name := range []string{"ToolResultStep", "toolAttemptStep", "approvalStep", "approvalDecisionStep", "ApprovalTallyStep", "sagaCompensateStep", "sagaArgsStep"} {
		for _, id := range adversarialToolUseIDs() {
			k := keyConstructors[name](id)
			if name == "approvalDecisionStep" {
				k = approvalDecisionStep(id, "ops", true, nil)
			}
			if len(k) > 256 || strings.IndexFunc(k, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
				t.Fatalf("%s(%.30q) = %.60q (%d bytes), not short printable ASCII", name, id, k, len(k))
			}
		}
	}
}

// A tool call's halt and a step's are resolved by their own functions: each refuses a name only
// the other kind of operation attempted, rather than record a result nothing reads.
func TestResolve_RefusesTheOtherKindsHalt(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	seedStepAttempt(t, store, "r", "reserve", time.Now())
	if err := ResolveHalt(ctx, store, "r", "reserve", "ok", false); !errors.Is(err, ErrConfig) {
		t.Fatalf("ResolveHalt on a step's halt = %v, want ErrConfig", err)
	}
	if _, _, err := ClaimAttempt(ctx, store, "r", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := ResolveStepHalt(ctx, store, "r", "c1", "ok", false); !errors.Is(err, ErrConfig) {
		t.Fatalf("ResolveStepHalt on a call's halt = %v, want ErrConfig", err)
	}
	// Each records its own kind of result: a step's is a step value (provable as a step).
	if err := ResolveStepHalt(ctx, store, "r", "reserve", "ok", false); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHalt(ctx, store, "r", "c1", "ok", false); err != nil {
		t.Fatal(err)
	}
	if rec, ok := hasStep(t, store, "r", "reserve"); !ok || rec.Kind != StepValue || rec.ToolUseID != "" {
		t.Fatalf("step result = %+v, want a StepValue", rec)
	}
	if rec, ok := hasStep(t, store, "r", ToolResultStep("c1")); !ok || rec.Kind != StepToolResult || rec.ToolUseID != "c1" {
		t.Fatalf("call result = %+v, want a StepToolResult for c1", rec)
	}
}

// The functions and constants named *Step that build a string are exactly keyConstructors.
func TestEngineKeys_ConstructorsAreListed(t *testing.T) {
	pkg := parseAgentPackage(t)
	found := map[string]bool{}
	for _, f := range pkg {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				r := d.Type.Results
				if d.Recv == nil && strings.HasSuffix(d.Name.Name, "Step") && r != nil && len(r.List) == 1 && len(r.List[0].Names) <= 1 && types.ExprString(r.List[0].Type) == "string" {
					found[d.Name.Name] = true
				}
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, s := range d.Specs {
					vs := s.(*ast.ValueSpec)
					if vs.Type != nil && types.ExprString(vs.Type) != "string" {
						continue // a typed constant of another type (OpStep is an OpKind), not a key
					}
					for _, n := range vs.Names {
						if strings.HasSuffix(n.Name, "Step") {
							found[n.Name] = true
						}
					}
				}
			}
		}
	}
	for n := range found {
		if keyConstructors[n] == nil {
			t.Errorf("%s builds a journal key but is not in keyConstructors", n)
		}
	}
	for n := range keyConstructors {
		if !found[n] {
			t.Errorf("keyConstructors lists %s, which the package does not declare", n)
		}
	}
}

// Every key the engine writes comes from a constructor: the name passed to Durable.Do,
// ClaimAttempt or step is a *Step call or constant, a local set from one, or a parameter of a
// function that only forwards a name its own callers are held to (listed in forwarders).
func TestEngineKeys_WritesUseConstructors(t *testing.T) {
	forwarders := map[string]bool{
		"ClaimAttempt:name":     true, // its callers are checked here
		"step:name":             true, // its callers are checked here
		"Step:name":             true, // a developer-chosen name, refused if reserved (checkStepName)
		"resolveHalt:h.result":  true, // ToolResultStep, or a step name checkStepName allowed
		"resolveHalt:heldKey":   true, // assigned from nextAttemptStep
		"claimAttempt:name":     true, // its callers are checked here
		"probe:key":             true, // its callers are checked here
		"doShared:key":          true, // its callers are checked here
		"step:markerKey":        true, // returned by claimNextAttempt, which builds it with retryAttemptStep
		"run:markerKey":         true, // returned by claimNextAttempt, which builds it with retryAttemptStep
		"putRecord:name":        true, // its callers are checked here
		"recordFresh:name":      true, // its callers are checked here
		"lookup:name":           true, // its callers are checked here
		"hasValueStep:name":     true, // its callers pass run:aborted
		"journalStep:name":      true, // step's name, forwarded
		"durableStep:name":      true, // step's name, forwarded
		"durableStep:markerKey": true, // returned by claimNextAttempt, which builds it with retryAttemptStep
		"Do:name":               true, // MemStore.Do forwards its caller's name to its Journal
		"init:name":             true, // journalhook.Do forwards audit's and plan's names
	}
	var writes int
	for file, f := range parseAgentPackage(t) {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name ast.Expr
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "Step" && fn.Name.Name != "Parallel" {
					// Step refuses the engine's reserved names; the engine's own steps use step.
					t.Errorf("%s: %s calls Step; an engine step must call step with a *Step key", file, fn.Name.Name)
				}
				switch f := call.Fun.(type) {
				case *ast.SelectorExpr:
					if f.Sel.Name == "Do" && len(call.Args) == 4 {
						name = call.Args[2]
					}
				case *ast.Ident:
					if attemptWriters[f.Name] && len(call.Args) >= 4 {
						name = call.Args[3]
					}
				}
				if name == nil {
					return true
				}
				writes++
				if !keyFromConstructor(name, fn, forwarders) {
					t.Errorf("%s: %s writes key %s, which is not built by a *Step constructor", file, fn.Name.Name, types.ExprString(name))
				}
				return true
			})
		}
	}
	if writes < 20 {
		t.Fatalf("found only %d key writes; the scan is not seeing the engine's calls", writes)
	}
}

// attemptWriters are the functions that write (or read by writing nothing) the key in their
// fourth argument; a call to any of them is checked like a call to Do.
var attemptWriters = map[string]bool{
	"ClaimAttempt": true, "step": true, "claimAttempt": true, "claimNextAttempt": true, "probe": true,
	"doShared": true, "voided": true, "liveAttempt": true, "recordNotStarted": true,
	"putRecord": true, "recordFresh": true, "lookup": true,
}

func keyFromConstructor(e ast.Expr, fn *ast.FuncDecl, forwarders map[string]bool) bool {
	switch e := e.(type) {
	case *ast.CallExpr:
		id, ok := e.Fun.(*ast.Ident)
		return ok && keyConstructors[id.Name] != nil
	case *ast.Ident:
		if keyConstructors[e.Name] != nil || forwarders[fn.Name.Name+":"+e.Name] {
			return true
		}
		// A local defined from a constructor.
		defined := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || as.Tok != token.DEFINE || len(as.Lhs) != len(as.Rhs) {
				return true
			}
			for i, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == e.Name && keyFromConstructor(as.Rhs[i], fn, forwarders) {
					defined = true
				}
			}
			return true
		})
		return defined
	default:
		return forwarders[fn.Name.Name+":"+types.ExprString(e)]
	}
}

// parseAgentPackage parses the package's non-test files.
func parseAgentPackage(t *testing.T) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[n] = f
	}
	return files
}

// ResolveStepHalt names a step, so it refuses a reserved name as Step does.
func TestResolveStepHalt_ReservedNameIsRefused(t *testing.T) {
	store := NewMemStore()
	err := ResolveStepHalt(context.Background(), store, "r", runCompleteStep, "ok", false)
	if complete, _ := IsComplete(context.Background(), store, "r"); !errors.Is(err, ErrConfig) || complete {
		t.Fatalf("err = %v, IsComplete = %v; want ErrConfig and no completion marker", err, complete)
	}
}

// RunSaga refuses a sub-run ID before it reads the journal, so it never rolls back a sub-agent's
// run on its own (a recorded saga failure sends RunSaga straight to rollback).
func TestRunSaga_SubRunIDIsRefusedBeforeRollback(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	sub := SubRunID("r", "c1")
	if _, err := store.Do(ctx, sub, "failed", func(context.Context) (Record, error) {
		return Record{Kind: StepSagaFail, ToolUseID: "x", Result: mustJSON("boom")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	a := New(NewScriptedModel(TextTurn("done")), store)
	for _, run := range []func() error{
		func() error { _, err := a.RunSaga(ctx, sub, "go"); return err },
		func() error { _, err := a.RunSagaResult(ctx, sub, "go"); return err },
	} {
		if err := run(); !errors.Is(err, ErrConfig) {
			t.Fatalf("err = %v, want ErrConfig", err)
		}
	}
	if aborted, _ := hasValueStep(ctx, store, sub, runAbortedStep); aborted {
		t.Fatal("the sub-run was rolled back and marked aborted")
	}
}
