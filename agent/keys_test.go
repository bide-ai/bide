package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"ApprovalPendingStep":        ApprovalPendingStep,
	"ApprovalIdentTallyStep":     func(s string) string { return ApprovalIdentTallyStep(s, 0) },
	"ApprovalIdentReaskStep":     func(s string) string { return ApprovalIdentReaskStep(s, 0) },
	"ApprovalIdentityStep":       func(s string) string { return ApprovalIdentityStep(s, 0) },
	"ApprovalIdentityDigestStep": func(s string) string { return ApprovalIdentityDigestStep(s, 0, "0123456789abcdef") },
	"ApprovalStatusStep":         ApprovalStatusStep,
	"ApprovalExecHoldStep":       func(s string) string { return ApprovalExecHoldStep(s, 0) },
	"ApprovalPauseStep":          func(s string) string { return ApprovalPauseStep(s, 1) },
	"ApprovalRequestStep":        ApprovalRequestStep,
	"ApprovalDeadlineStep":       ApprovalDeadlineStep,
	"approvalAfterRequestStep":   approvalAfterRequestStep,
	"stopRecordStep":             func(s string) string { return stopRecordStep(StopRecord{Kind: "set", StopID: s}) },
	"runCompleteStep":            func(string) string { return runCompleteStep },
	"runAbortedStep":             func(string) string { return runAbortedStep },
	"runStartStep":               func(string) string { return runStartStep },
	"runPrincipalStep":           func(string) string { return runPrincipalStep },
	"runCancelledStep":           func(string) string { return runCancelledStep },
	"runCancelRequestedStep":     func(string) string { return runCancelRequestedStep },
	"runLimitsStep":              func(s string) string { return runLimitsStep(len(s)) },
	"headerStep":                 func(string) string { return headerStep },
	"modelStep":                  func(s string) string { return modelStep(len(s)) },
	"ToolResultStep":             ToolResultStep,
	"toolAttemptStep":            toolAttemptStep,
	"stepAttemptStep":            stepAttemptStep,
	"retryAttemptStep":           func(s string) string { return retryAttemptStep(toolAttemptStep(s), 1+len(s)) },
	"notStartedStep":             func(s string) string { return notStartedStep(toolAttemptStep(s), "0123abcd") },
	"nextAttemptStep":            func(s string) string { return nextAttemptStep(retryAttemptStep(stepAttemptStep(s), 1+len(s))) },
	"approvalStep":               approvalStep,
	"approvalDecisionStep": func(s string) string {
		return approvalDecisionStep(s, "ops:1", true, "ed25519", "2026-10-13T12:14:03.123Z", []byte(s))
	},
	"decisionIdentityStep": func(s string) string {
		return decisionIdentityStep(s, "ops:3", true, "ed25519", "2026-10-13T12:14:03.123Z", nil)
	},
	"DecisionStep": func(s string) string {
		return DecisionStep(Decision{ToolUseID: s, ApproverID: "ops:2", Approved: true, Alg: "ed25519",
			DecidedAt: time.Date(2026, 10, 13, 12, 14, 3, 123e6, time.UTC), Signature: []byte(s)})
	},
	"ApprovalTallyStep":      ApprovalTallyStep,
	"ApprovalRetallyStep":    ApprovalRetallyStep,
	"ApprovalSoDRetallyStep": ApprovalSoDRetallyStep,
	"ApprovalReaskStep":      func(s string) string { return ApprovalReaskStep(s, len(s)%2 == 0) },
	"sagaCompensateStep":     sagaCompensateStep,
	"sagaArgsStep":           sagaArgsStep,
	"subRunLinkStep":         func(s string) string { return subRunLinkStep(s, s) },
	"signalStep":             signalStep,
	"awaitTimeoutStep":       awaitTimeoutStep,
	"awaitResolvedStep":      awaitResolvedStep,
	"timerStep":              timerStep,
	"interruptStep":          interruptStep,
	"chanStep":               func(s string) string { return chanStep(s, s) },
	"chanAckStep":            func(s string) string { return chanAckStep(s, s) },
	"sessionTurnStep":        func(s string) string { return sessionTurnStep(len(s)) },
	"sessionStartStep":       func(s string) string { return sessionStartStep(len(s)) },
	"sessionFromStep":        sessionFromStep,
	"sessionTaintStep":       func(string) string { return sessionTaintStep },
	"retrievalStep":          func(s string) string { return retrievalStep(len(s)) },
	"planScopedStep": func(s string) string {
		return planScopedStep(context.WithValue(context.Background(), planScopeKey{}, planScope{runID: "r", node: "node:n"}), "r", s)
	},
	"gateReasonStep": gateReasonStep,
	"spendStep":      spendStep,
	"lateSpendStep":  lateSpendStep,
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
			if name == "runCompleteStep" || name == "runAbortedStep" || name == "runStartStep" || name == "runPrincipalStep" || name == "runCancelledStep" || name == "runCancelRequestedStep" || name == "headerStep" || name == "sessionTaintStep" {
				from = name // a constant
			}
			if name == "sessionTurnStep" || name == "sessionStartStep" || name == "modelStep" || name == "retrievalStep" || name == "runLimitsStep" {
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
	for _, name := range []string{"ToolResultStep", "toolAttemptStep", "approvalStep", "approvalDecisionStep", "decisionIdentityStep", "ApprovalTallyStep", "ApprovalRetallyStep", "ApprovalSoDRetallyStep", "ApprovalPendingStep", "ApprovalReaskStep", "sagaCompensateStep", "sagaArgsStep"} {
		for _, id := range adversarialToolUseIDs() {
			k := keyConstructors[name](id)
			if name == "approvalDecisionStep" {
				k = approvalDecisionStep(id, "ops", true, "ed25519", "2026-10-13T12:14:03.123Z", nil)
			}
			if name == "decisionIdentityStep" {
				k = decisionIdentityStep(id, "ops", true, "ed25519", "2026-10-13T12:14:03.123Z", nil)
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
	store := memJournal()
	seedStepAttempt(t, store, "r", "reserve", time.Now())
	if err := ResolveHalt(ctx, store, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "reserve"}, Cause: HaltCrashed}, Outcome{Result: "ok", IsError: false}); !errors.Is(err, ErrConfig) {
		t.Fatalf("ResolveHalt on a step's halt = %v, want ErrConfig", err)
	}
	if _, _, err := ClaimAttempt(ctx, store, "r", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHalt(ctx, store, HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "ok", IsError: false}); !errors.Is(err, ErrConfig) {
		t.Fatalf("ResolveHalt (OpStep) on a call's halt = %v, want ErrConfig", err)
	}
	// Each records its own kind of result: a step's is a step value (provable as a step).
	if err := ResolveHalt(ctx, store, HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: "reserve"}, Cause: HaltCrashed}, Outcome{Result: "ok", IsError: false}); err != nil {
		t.Fatal(err)
	}
	if err := ResolveHalt(ctx, store, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "ok", IsError: false}); err != nil {
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

// Every key the engine writes comes from a constructor: the name passed to a Journal's write method,
// ClaimAttempt or step is a *Step call or constant, a local set from one, or a parameter of a
// function that only forwards a name its own callers are held to (listed in forwarders).
func TestEngineKeys_WritesUseConstructors(t *testing.T) {
	forwarders := map[string]bool{
		"ClaimAttempt:name":      true, // its callers are checked here
		"submitDecision:name":    true, // decisionIdentityStep, or approvalDecisionStep for a decision whose identity name a bad record holds
		"retryNotStarted:key":    true, // a live marker's own key, read from the journal, as Journal.retryNotStarted takes it
		"step:name":              true, // its callers are checked here
		"Step:name":              true, // a developer-chosen name, refused if reserved (checkStepName)
		"resolveHalt:h.result":   true, // ToolResultStep, or a step name checkStepName allowed
		"resolveHalt:heldKey":    true, // assigned from nextAttemptStep
		"claimAttempt:name":      true, // its callers are checked here
		"claimNextAttempt:base":  true, // its callers are checked here (it forwards to Journal.claimNext)
		"probe:key":              true, // its callers are checked here
		"doShared:key":           true, // its callers are checked here
		"step:markerKey":         true, // returned by claimNextAttempt, which builds it with retryAttemptStep
		"runLoop:markerKey":      true, // returned by claimNextAttempt, which builds it with retryAttemptStep
		"putRecord:name":         true, // its callers are checked here
		"writeEnd:name":          true, // its callers are checked here
		"cancelSeen:p.cancelKey": true, // runCancelledStep or runCancelRequestedStep (openPlan)
		"postClaim:markerKey":    true, // returned by claimNextAttempt, which builds it with retryAttemptStep
		"recordFresh:name":       true, // its callers are checked here
		"lookup:name":            true, // its callers are checked here
		"hasValueStep:name":      true, // its callers pass run:aborted
		"journalStep:name":       true, // step's name, forwarded
		"durableStep:name":       true, // step's name, forwarded
		"durableStep:markerKey":  true, // returned by claimNextAttempt, which builds it with retryAttemptStep
		"Do:name":                true, // MemStore.Do forwards its caller's name to its Journal
		"init:name":              true, // journalhook.Do forwards audit's and plan's names
		"recordSpend:name":       true, // its callers pass spendStep and lateSpendStep
		"Insert:name":            true, // capJournal.Insert forwards the names internal/capledger builds (slot:, win:, cap-intent:, cap-reserve:)
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
					if journalWriters[f.Sel.Name] && len(call.Args) == 4 {
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

// journalWriters are the Journal's methods that write the key in their third argument (ctx, run
// ID, key, then the record or the function that makes it): every engine write goes through one.
var journalWriters = map[string]bool{"do": true, "doFresh": true, "put": true, "putNew": true, "claim": true, "claimNext": true}

// attemptWriters are the functions that write (or read by writing nothing) the key in their
// fourth argument; a call to any of them is checked like a call to Do.
var attemptWriters = map[string]bool{
	"ClaimAttempt": true, "step": true, "claimAttempt": true, "claimNextAttempt": true, "probe": true,
	"doShared": true, "voided": true, "liveAttempt": true, "recordNotStarted": true,
	"putRecord": true, "recordFresh": true, "lookup": true, "writeEnd": true,
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

// ResolveHalt (OpStep) names a step, so it refuses a reserved name as Step does.
func TestResolveHalt_StepReservedNameIsRefused(t *testing.T) {
	store := memJournal()
	err := ResolveHalt(context.Background(), store, HaltRef{RunID: "r", Op: OpRef{Kind: OpStep, ID: runCompleteStep}, Cause: HaltCrashed}, Outcome{Result: "ok", IsError: false})
	if complete, _ := IsComplete(context.Background(), store, "r"); !errors.Is(err, ErrConfig) || complete {
		t.Fatalf("err = %v, IsComplete = %v; want ErrConfig and no completion marker", err, complete)
	}
}

// saga Run refuses a sub-run ID before it reads the journal, so it never rolls back a sub-agent's
// run on its own (a recorded saga failure sends saga Run straight to rollback).
func TestSagaRun_SubRunIDIsRefusedBeforeRollback(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	j := mustJournal(store)
	sub := SubRunID("r", "c1")
	if _, err := j.do(ctx, sub, "failed", func(context.Context) (Record, error) {
		return Record{Kind: StepSagaFail, ToolUseID: "x", Result: mustJSON("boom")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	a := mustNew(NewScriptedModel(TextTurn("done")), j)
	for _, run := range []func() error{
		func() error { _, err := a.Run(ctx, sub, UserText("go"), WithSaga()); return err },
		func() error { _, err := a.Run(ctx, sub, UserText("go"), WithSaga()); return err },
	} {
		if err := run(); !errors.Is(err, ErrConfig) {
			t.Fatalf("err = %v, want ErrConfig", err)
		}
	}
	if aborted, _ := hasValueStep(ctx, j, sub, runAbortedStep); aborted {
		t.Fatal("the sub-run was rolled back and marked aborted")
	}
}

// overlappingDo runs Do for (run1, name1) with a step that waits, then, while it waits, Do for
// (run2, name2), and returns the record each call got back and whether the second step ran. The
// first step is held until the second call has had time to reach the store, so a store that
// keyed the two steps alike would hand the second caller the first step's record.
func overlappingDo(t *testing.T, d *Journal, run1, name1, run2, name2 string) (first, second Record, secondRan bool) {
	t.Helper()
	ctx := context.Background()
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan Record, 1)
	go func() {
		rec, err := d.do(ctx, run1, name1, func(context.Context) (Record, error) {
			close(started)
			<-release
			return Record{Kind: StepValue, Result: json.RawMessage(`"first"`)}, nil
		})
		if err != nil {
			t.Errorf("Do(%q, %q): %v", run1, name1, err)
		}
		done <- rec
	}()
	<-started
	secondDone := make(chan Record, 1)
	go func() {
		rec, err := d.do(ctx, run2, name2, func(context.Context) (Record, error) {
			secondRan = true
			return Record{Kind: StepValue, Result: json.RawMessage(`"second"`)}, nil
		})
		if err != nil {
			t.Errorf("Do(%q, %q): %v", run2, name2, err)
		}
		secondDone <- rec
	}()
	select {
	case second = <-secondDone: // the second call did not wait on the first: nothing shared
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	first = <-done
	if second.Name == "" {
		second = <-secondDone
	}
	return first, second, secondRan
}

// Two different steps are never one step, whatever bytes their run IDs and names hold. The
// store's in-process deduplication key must tell ("a\x00b", "c") from ("a", "b\x00c"): keyed
// alike, a concurrent Do of the second would share the first's fn and get its record back.
func TestMemStore_DistinctStepsWithNULDoNotShareADo(t *testing.T) {
	for _, c := range [][4]string{{"a\x00b", "c", "a", "b\x00c"}, {"ab", "c", "a", "bc"}} {
		first, second, ran := overlappingDo(t, memJournal(), c[0], c[1], c[2], c[3])
		if string(first.Result) != `"first"` {
			t.Errorf("%q: first step's record = %s, want \"first\"", c, first.Result)
		}
		if !ran || string(second.Result) != `"second"` || second.Name != c[3] {
			t.Errorf("%q: second step ran=%v, record %q %s; want its own fn run and record \"second\"", c, ran, second.Name, second.Result)
		}
	}
}

// Two timers of two runs are two wakes, whatever bytes the run IDs and timer names hold:
// ("a\x00b", "c") and ("a", "b\x00c") keyed alike would let one registration overwrite the
// other, and the overwritten run would never wake.
func TestMemWaker_DistinctTimersWithNULAreKeptApart(t *testing.T) {
	woke := map[string]bool{}
	w := NewMemWaker(func(_ context.Context, runID string) error {
		woke[runID] = true
		return nil
	})
	at := time.Unix(100, 0)
	w.Schedule(context.Background(), Wake{RunID: "a\x00b", Name: "c", FireAt: at})
	w.Schedule(context.Background(), Wake{RunID: "a", Name: "b\x00c", FireAt: at})
	w.Schedule(context.Background(), Wake{RunID: "xy", Name: "z", FireAt: at})
	w.Schedule(context.Background(), Wake{RunID: "x", Name: "yz", FireAt: at})
	if n, err := w.Fire(context.Background(), at); err != nil || n != 4 {
		t.Fatalf("Fire = %d, %v; want both runs resumed", n, err)
	}
	if !woke["a\x00b"] || !woke["a"] || !woke["xy"] || !woke["x"] {
		t.Fatalf("woke %v; want all four runs", woke)
	}
}

// memTamper replaces the stored encoding of step name in run runID with data, as a corrupted or
// edited backing store would.
func memTamper(t *testing.T, m *MemStore, runID, name string, data []byte) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	rl := m.runs[runID]
	i, ok := rl.byName[name]
	if !ok {
		t.Fatalf("no step %q in run %s", name, runID)
	}
	rl.entries[i].Data = data
}

// A row's key is the name it was recorded under, and the record it holds carries that name. A
// row whose record names another step (a row copied or edited in the backing store) must not be
// read as that other step: the loop and the recovery supervisor find records by their name, so a
// row under "x" holding a record named run:complete would mark an unfinished run complete.
func TestMemStore_RowWhoseRecordNamesAnotherStepIsRefused(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	j := mustJournal(m)
	if _, err := j.do(ctx, "r", "x", func(context.Context) (Record, error) { return Record{Kind: StepValue, Result: []byte(`1`)}, nil }); err != nil {
		t.Fatal(err)
	}
	forged, err := JournalEntry(runCompleteStep, Record{Kind: StepValue})
	if err != nil {
		t.Fatal(err)
	}
	memTamper(t, m, "r", "x", forged)

	if done, err := IsComplete(ctx, j, "r"); !errors.Is(err, ErrStorage) {
		t.Errorf("IsComplete = %v, %v; want an ErrStorage error, not a run marked complete by a misfiled row", done, err)
	}
	if _, err := j.History(ctx, "r"); !errors.Is(err, ErrStorage) || !strings.Contains(err.Error(), `"x"`) {
		t.Errorf("History = %v; want ErrStorage naming the row's key", err)
	}
	ran := false
	if _, err := j.do(ctx, "r", "x", func(context.Context) (Record, error) { ran = true; return Record{}, nil }); !errors.Is(err, ErrStorage) {
		t.Errorf("Do(memoized) = %v; want ErrStorage", err)
	}
	if ran {
		t.Error("Do ran fn for a recorded step")
	}
}

// A record with a field this version does not know (written by a newer version) still reads: the
// name check does not make the journal decoding strict.
func TestMemStore_UnknownFieldStillReads(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	j := mustJournal(m)
	if _, err := j.do(ctx, "r", "x", func(context.Context) (Record, error) { return Record{Kind: StepValue, Result: []byte(`1`)}, nil }); err != nil {
		t.Fatal(err)
	}
	memTamper(t, m, "r", "x", []byte(`{"name":"x","kind":"value","result":1,"future_field":{"a":1}}`))
	recs, err := j.History(ctx, "r")
	if err != nil || len(recs) != 2 || recs[1].Name != "x" { // the header, then the record
		t.Fatalf("History = %+v, %v; want the one record", recs, err)
	}
	if rec, err := j.do(ctx, "r", "x", func(context.Context) (Record, error) { return Record{}, errors.New("must not run") }); err != nil || string(rec.Result) != "1" {
		t.Fatalf("Do = %+v, %v", rec, err)
	}
}

// A row that does not decode at all is reported as such (with its run and key), not as a record
// under another name.
func TestMemStore_UndecodableRowIsReportedAsUndecodable(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	j := mustJournal(m)
	if _, err := j.do(ctx, "r", "x", func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil }); err != nil {
		t.Fatal(err)
	}
	memTamper(t, m, "r", "x", []byte(`not json`))
	_, err := j.History(ctx, "r")
	if !errors.Is(err, ErrStorage) || !strings.Contains(err.Error(), "decode stored record") || !strings.Contains(err.Error(), `"x"`) {
		t.Fatalf("History = %v; want the decode failure, naming the key", err)
	}
}
