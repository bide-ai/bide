package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// The option scopes are enforced by the type checker, which is what this test runs: each snippet
// is type-checked against the compiled agent package, as go vet would check a caller. An option
// passed where it does not apply must not compile, every combination type must fit each of its
// scopes, the closed list of combination types is exactly the five api-v1.md names, and every
// option constructor returns its narrowest type.
func TestOptionScopes_TypeChecked(t *testing.T) {
	pkg, imp := loadAgent(t)

	// Snippets that must compile.
	for name, body := range map[string]string{
		"AgentRunOption fits Option and RunOption": `
			var _ agent.Option = agent.WithMaxTurns(1)
			var _ agent.RunOption = agent.WithMaxTurns(1)
			_ = []agent.Option{agent.WithTokenBudget(1), agent.WithSystemPrompt(""), agent.WithSampling(), agent.WithToolChoice(agent.ToolChoice{}), agent.WithWaker(nil), agent.WithIdentity(agent.Identity{})}
			_ = []agent.RunOption{agent.WithTokenBudget(1), agent.WithSystemPrompt(""), agent.WithSampling(), agent.WithToolChoice(agent.ToolChoice{}), agent.WithWaker(nil), agent.WithIdentity(agent.Identity{})}`,
		"ConcurrencyOption fits Option, RunOption and ParallelOption": `
			_ = []agent.Option{agent.WithMaxConcurrency(1)}
			_ = []agent.RunOption{agent.WithMaxConcurrency(1)}
			_ = []agent.ParallelOption{agent.WithMaxConcurrency(1)}`,
		"ClockOption fits Option, RunOption and ResolveOption": `
			_ = []agent.Option{agent.WithClock(nil)}
			_ = []agent.RunOption{agent.WithClock(nil)}
			_ = []agent.ResolveOption{agent.WithClock(nil), agent.WithMinHaltAge(0)}`,
		"SafetyOption fits ToolOption and StepOption": `
			_ = []agent.ToolOption{agent.WithSafety(agent.Safety{}), agent.WithTimeout(1)}
			_ = []agent.StepOption{agent.WithSafety(agent.Safety{})}`,
		"LeaseControl fits LeaseOption, RecoverOption and RecoverLoopOption": `
			_ = []agent.LeaseOption{agent.WithLeaseHolder(""), agent.WithLeaseTTL(1)}
			_ = []agent.RecoverOption{agent.WithLeaseHolder(""), agent.WithLeaseTTL(1)}
			_ = []agent.RecoverLoopOption{agent.WithLeaseHolder(""), agent.WithLeaseTTL(1), agent.WithRecoverInterval(1)}`,
		"ReanchorOption fits RecoverOption and RecoverLoopOption": `
			_ = []agent.RecoverOption{agent.WithReanchor(nil), agent.WithReanchorTimeout(1)}
			_ = []agent.RecoverLoopOption{agent.WithReanchor(nil), agent.WithReanchorTimeout(1), agent.WithRecoverInterval(1)}`,
		"agent-only options": `
			_ = []agent.Option{agent.WithTools(), agent.WithMiddleware(), agent.WithToolMiddleware(), agent.WithApproverVerifiers(nil), agent.WithStopSwitch(nil),
				agent.WithToolErrorRedactor(nil), agent.WithRetrieval(nil, 1), agent.WithSystemPromptFunc(nil), agent.WithOptions()}`,
		"options built conditionally": `
			var opts []agent.Option
			if true { opts = append(opts, agent.WithMaxTurns(3)) }
			opts = append(opts, agent.WithTools())
			_ = opts`,
	} {
		if err := typeCheck(imp, body); err != nil {
			t.Errorf("%s: does not compile: %v", name, err)
		}
	}

	// Snippets that must not compile, each with the scope the option lacks.
	for name, tc := range map[string]struct{ body, lacks string }{
		"WithMaxTurns as a ParallelOption":                  {`var _ agent.ParallelOption = agent.WithMaxTurns(1)`, "applyParallel"},
		"WithMaxTurns as a ResolveOption":                   {`var _ agent.ResolveOption = agent.WithMaxTurns(1)`, "applyResolve"},
		"WithMaxTurns as a ToolOption":                      {`var _ agent.ToolOption = agent.WithMaxTurns(1)`, "applyTool"},
		"WithMaxTurns as a StepOption":                      {`var _ agent.StepOption = agent.WithMaxTurns(1)`, "applyStep"},
		"WithMaxTurns as a LeaseOption":                     {`var _ agent.LeaseOption = agent.WithMaxTurns(1)`, "applyLease"},
		"WithIdentity as a ParallelOption":                  {`var _ agent.ParallelOption = agent.WithIdentity(agent.Identity{})`, "applyParallel"},
		"WithTools as a RunOption":                          {`var _ agent.RunOption = agent.WithTools()`, "applyRun"},
		"WithRetrieval as a RunOption":                      {`var _ agent.RunOption = agent.WithRetrieval(nil, 1)`, "applyRun"},
		"WithSystemPromptFunc as a RunOption":               {`var _ agent.RunOption = agent.WithSystemPromptFunc(nil)`, "applyRun"},
		"WithOptions as a RunOption":                        {`var _ agent.RunOption = agent.WithOptions()`, "applyRun"},
		"WithMaxConcurrency as a ResolveOption":             {`var _ agent.ResolveOption = agent.WithMaxConcurrency(1)`, "applyResolve"},
		"WithMaxConcurrency as a StepOption":                {`var _ agent.StepOption = agent.WithMaxConcurrency(1)`, "applyStep"},
		"WithClock as a ParallelOption":                     {`var _ agent.ParallelOption = agent.WithClock(nil)`, "applyParallel"},
		"WithClock as a StepOption":                         {`var _ agent.StepOption = agent.WithClock(nil)`, "applyStep"},
		"WithSafety as an Option":                           {`var _ agent.Option = agent.WithSafety(agent.Safety{})`, "applyAgent"},
		"WithSafety as a RunOption":                         {`var _ agent.RunOption = agent.WithSafety(agent.Safety{})`, "applyRun"},
		"WithSafety as a ParallelOption":                    {`var _ agent.ParallelOption = agent.WithSafety(agent.Safety{})`, "applyParallel"},
		"WithTimeout as a StepOption":                       {`var _ agent.StepOption = agent.WithTimeout(1)`, "applyStep"},
		"WithLeaseTTL as an Option":                         {`var _ agent.Option = agent.WithLeaseTTL(1)`, "applyAgent"},
		"WithLeaseHolder as a RunOption":                    {`var _ agent.RunOption = agent.WithLeaseHolder("")`, "applyRun"},
		"WithRecoverInterval as a RecoverOption":            {`var _ agent.RecoverOption = agent.WithRecoverInterval(1)`, "applyRecover"},
		"WithRecoverErrors as a LeaseOption":                {`var _ agent.LeaseOption = agent.WithRecoverErrors(nil)`, "applyLease"},
		"WithMinHaltAge as an Option":                       {`var _ agent.Option = agent.WithMinHaltAge(1)`, "applyAgent"},
		"WithEvidence as a RunOption":                       {`var _ agent.RunOption = agent.WithEvidence(nil)`, "applyRun"},
		"WithRecoverConcurrency in Recover's options":       {`_ = []agent.RecoverOption{agent.WithRecoverConcurrency(1)}`, "applyRecover"},
		"WithRecoverLapsedConcurrency in Recover's options": {`_ = []agent.RecoverOption{agent.WithRecoverLapsedConcurrency(1)}`, "applyRecover"},
		"an Option in a RunOption slice":                    {`_ = []agent.RunOption{agent.WithMaxTurns(1), agent.WithTools()}`, "applyRun"},
		"an option type outside the package": {`
			type mine struct{}
			var _ agent.Option = mine{}`, "applyAgent"},
	} {
		err := typeCheck(imp, tc.body)
		if err == nil {
			t.Errorf("%s: compiles, want a type error", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.lacks) {
			t.Errorf("%s: type error %q does not name the missing scope method %s", name, err, tc.lacks)
		}
	}

	// The closed list: the exported interfaces that combine two or more option scopes.
	scopes := []string{"Option", "RunOption", "ParallelOption", "StepOption", "ResolveOption", "ToolOption",
		"LeaseOption", "RecoverOption", "RecoverLoopOption", "RetrievalOption"}
	var combos []string
	for _, name := range pkg.Scope().Names() {
		obj, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok || !obj.Exported() {
			continue
		}
		if _, ok := obj.Type().Underlying().(*types.Interface); !ok || slices.Contains(scopes, name) {
			continue
		}
		n := 0
		for _, s := range scopes {
			if types.AssignableTo(obj.Type(), pkg.Scope().Lookup(s).Type()) {
				n++
			}
		}
		if n >= 2 {
			combos = append(combos, name)
		}
	}
	slices.Sort(combos)
	if want := []string{"AgentRunOption", "ClockOption", "ConcurrencyOption", "LeaseControl", "NotifierOption", "ReanchorOption", "SafetyOption"}; !slices.Equal(combos, want) {
		t.Errorf("combination types = %v, want the closed list %v", combos, want)
	}

	// Every option constructor returns its narrowest type, and a new one must be listed here.
	want := map[string]string{
		"WithMaxTurns": "AgentRunOption", "WithTokenBudget": "AgentRunOption", "WithSystemPrompt": "AgentRunOption",
		"WithSampling": "AgentRunOption", "WithToolChoice": "AgentRunOption", "WithWaker": "AgentRunOption",
		"WithIdentity":       "AgentRunOption",
		"WithMaxConcurrency": "ConcurrencyOption",
		"WithSaga":           "RunOption", "WithToolFilter": "RunOption", "WithOutputMode": "RunOption",
		"WithClock":       "ClockOption",
		"WithNotifier":    "NotifierOption",
		"WithSafety":      "SafetyOption",
		"WithLeaseHolder": "LeaseControl", "WithLeaseTTL": "LeaseControl",
		"WithTools": "Option", "WithMiddleware": "Option", "WithToolMiddleware": "Option", "WithApproverVerifiers": "Option", "WithStopSwitch": "Option",
		"WithToolErrorRedactor": "Option", "WithRetrieval": "Option", "WithSystemPromptFunc": "Option", "WithOptions": "Option",
		"WithRecoverInterval": "RecoverLoopOption", "WithRecoverConcurrency": "RecoverLoopOption", "WithRecoverLapsedConcurrency": "RecoverLoopOption", "WithRecoverErrors": "RecoverLoopOption", "WithReanchor": "ReanchorOption", "WithReanchorTimeout": "ReanchorOption",
		"WithMinHaltAge": "ResolveOption", "WithoutLiveDriverCheck": "ResolveOption", "WithEvidence": "ResolveOption",
		"WithRetrievalRetry": "RetrievalOption",
		"WithApproval":       "ToolOption", "WithTimeout": "ToolOption", "WithTitle": "ToolOption", "WithOutputSchema": "ToolOption", "WithSubRuns": "ToolOption",
	}
	optionTypes := append(slices.Clone(scopes), "AgentRunOption", "ConcurrencyOption", "ClockOption", "SafetyOption", "LeaseControl", "NotifierOption", "ReanchorOption")
	for _, name := range pkg.Scope().Names() {
		fn, ok := pkg.Scope().Lookup(name).(*types.Func)
		if !ok || !fn.Exported() {
			continue
		}
		res := fn.Type().(*types.Signature).Results()
		if res.Len() != 1 {
			continue
		}
		named, ok := res.At(0).Type().(*types.Named)
		if !ok || named.Obj().Pkg() != pkg || !slices.Contains(optionTypes, named.Obj().Name()) {
			continue
		}
		got := named.Obj().Name()
		switch w, listed := want[name]; {
		case !listed:
			t.Errorf("option constructor %s (returns %s) is not in the scope table; add it with its narrowest type", name, got)
		case w != got:
			t.Errorf("%s returns %s, want %s", name, got, w)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("option constructor %s is listed but not found", name)
	}
}

// typeCheck type-checks body as the body of a function in a file that imports the agent package.
func typeCheck(imp types.Importer, body string) error {
	src := "package p\n\nimport \"github.com/bide-ai/bide/agent\"\n\nvar _ = agent.New\n\nfunc _() {\n" + body + "\n}\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		return err
	}
	var first error
	conf := types.Config{Importer: imp, Error: func(err error) {
		if first == nil {
			first = err
		}
	}}
	_, _ = conf.Check("p", fset, []*ast.File{f}, nil)
	return first
}

// loadAgent returns the type-checked agent package and an importer that resolves it, and every
// package it imports, from the compiler's export data, which `go list -export` builds.
func loadAgent(t *testing.T) (*types.Package, types.Importer) {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Export", "github.com/bide-ai/bide/agent")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -export: %v", err)
	}
	exports := map[string]string{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p struct{ ImportPath, Export string }
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
	}
	imp := importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, os.ErrNotExist
		}
		return os.Open(file)
	})
	pkg, err := imp.Import("github.com/bide-ai/bide/agent")
	if err != nil {
		t.Fatal(err)
	}
	return pkg, imp
}

// Option constructors outside the agent package (trace.Instrument, and any a later package or
// nested module adds) follow the same rule: each returns its narrowest agent option type, and a
// new one must be listed here. Every module's source in the repository is read, nested modules
// included, since the agent package's tests cannot load their export data; the result type is
// resolved through each file's import of the agent package, whatever its local name.
func TestOptionScopes_ConstructorsOutsideAgent(t *testing.T) {
	want := map[string]string{
		"github.com/bide-ai/bide/trace.Instrument": "Option",
	}
	optionTypes := []string{"Option", "RunOption", "ParallelOption", "StepOption", "ResolveOption", "ToolOption",
		"LeaseOption", "RecoverOption", "RecoverLoopOption", "RetrievalOption",
		"AgentRunOption", "ConcurrencyOption", "ClockOption", "SafetyOption", "LeaseControl", "ReanchorOption"}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case path == filepath.Join(root, "agent"): // the test above checks it with the type checker
				return filepath.SkipDir
			case path != root && (d.Name() == "testdata" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		agentName := ""
		for _, im := range f.Imports {
			if im.Path.Value == `"github.com/bide-ai/bide/agent"` {
				agentName = "agent"
				if im.Name != nil {
					agentName = im.Name.Name
				}
			}
		}
		if agentName == "" {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		pkgPath := strings.TrimSuffix("github.com/bide-ai/bide/"+filepath.ToSlash(rel), "/.")
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
				continue
			}
			sel, ok := fn.Type.Results.List[0].Type.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != agentName || !slices.Contains(optionTypes, sel.Sel.Name) {
				continue
			}
			found++
			name := pkgPath + "." + fn.Name.Name
			switch w, listed := want[name]; {
			case !listed:
				t.Errorf("option constructor %s (returns agent.%s) is not in the table; add it with its narrowest type", name, sel.Sel.Name)
			case w != sel.Sel.Name:
				t.Errorf("%s returns agent.%s, want agent.%s", name, sel.Sel.Name, w)
			}
			delete(want, name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name := range want {
		t.Errorf("option constructor %s is listed but not found", name)
	}
	if found == 0 {
		t.Fatal("no option constructor found outside the agent package; is the repository root right?")
	}
}

// notifyDecorator is a decorator written for the old Tool method set: it embeds the tool it
// decorates, adds a side effect of its own, and declares itself a side effect through the old
// Safety method. The agent reads only Spec, which it promotes from the embedded tool
// (Idempotent), so the override would be dead and a resume would fire the decorator again.
type notifyDecorator struct{ agent.Tool }

func (notifyDecorator) Safety() agent.Safety { return agent.Safety{} }

// renamedDecorator overrides the name the old way.
type renamedDecorator struct{ agent.Tool }

func (renamedDecorator) Name() string { return "other" }

// agreeing has old methods that say what its Spec says: harmless helpers.
type agreeing struct{ agent.Tool }

func (a agreeing) Name() string { return a.Tool.Spec().Name }

// specDecorator overrides the way the new API does: in its Spec.
type specDecorator struct{ agent.Tool }

func (d specDecorator) Spec() agent.ToolSpec {
	s := d.Tool.Spec()
	s.Safety = agent.Safety{}
	return s
}

func idempotentLookup() agent.Tool {
	return agent.MustFunc("lookup", "look up", func(context.Context, struct{}) (string, error) { return "ok", nil },
		agent.WithSafety(agent.Safety{Idempotent: true}))
}

// A tool whose old-method-set method disagrees with its Spec is refused when the agent is built:
// the agent reads only Spec, so the method's value would be silently ignored (a side effect
// declared through Safety() re-run on resume, a name the model is never shown).
func TestNew_RefusesAnOldMethodThatDisagreesWithSpec(t *testing.T) {
	for name, tool := range map[string]agent.Tool{
		"Safety": notifyDecorator{idempotentLookup()},
		"Name":   renamedDecorator{idempotentLookup()},
	} {
		a, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(), agent.WithTools(tool))
		if !errors.Is(err, agent.ErrConfig) || a != nil {
			t.Errorf("%s override: New = %v, %v; want nil and ErrConfig", name, a, err)
		}
	}
	for name, tool := range map[string]agent.Tool{
		"agreeing helper": agreeing{idempotentLookup()},
		"Spec override":   specDecorator{idempotentLookup()},
	} {
		if _, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(), agent.WithTools(tool)); err != nil {
			t.Errorf("%s: New = %v, want accepted", name, err)
		}
	}
}

// pointer receiver Safety, registered as a value
type ptrRecv struct{ agent.Tool }

func (*ptrRecv) Safety() agent.Safety { return agent.Safety{} }

// nested: outer embeds a middle decorator that carries the dead override
type middle struct{ agent.Tool }

func (middle) Safety() agent.Safety { return agent.Safety{} }

type outer struct{ middle }

// Safety from a different embedded type (a mixin), Spec from the tool
type sideEffectMixin struct{}

func (sideEffectMixin) Safety() agent.Safety { return agent.Safety{} }

type mixed struct {
	agent.Tool
	sideEffectMixin
}

// two embedded fields declare Safety: neither is promoted, so the override is lost silently
type readOnlyMixin struct{}

func (readOnlyMixin) Safety() agent.Safety { return agent.Safety{ReadOnly: true} }

type ambiguous struct {
	agent.Tool
	sideEffectMixin
	readOnlyMixin
}

// Safety returning a named type convertible to agent.Safety? (not the same method signature)
type mySafety agent.Safety
type otherSig struct{ agent.Tool }

func (otherSig) Safety() mySafety { return mySafety{} }

// legit: a decorator that overrides Spec and keeps old methods consistent
type legit struct{ agent.Tool }

func (d legit) Spec() agent.ToolSpec { s := d.Tool.Spec(); s.Safety = agent.Safety{}; return s }
func (legit) Safety() agent.Safety   { return agent.Safety{} }

// legit: Name() meaning something else entirely (an unrelated interface)
type displayNamed struct{ agent.Tool }

func (displayNamed) Name() string { return "display name" }

// Call-only decorator with a json schema override via ArgsSchema returning equal-but-reformatted JSON
type reformatted struct{ agent.Tool }

func (d reformatted) ArgsSchema() json.RawMessage {
	var v any
	_ = json.Unmarshal(d.Tool.Spec().Input, &v)
	b, _ := json.MarshalIndent(v, "", " ")
	return b
}

// Every shape in which an old method would be dead is refused, and the shapes that are not
// dead are accepted: a pointer-receiver Safety on a tool registered by value, an override nested
// one embedding down, an override a mixin declares, one two embedded fields declare (neither promoted), a Safety of
// another signature; a Spec that says what the old methods say, and an ArgsSchema that is the
// same JSON reformatted. A Name() that means something else is refused too, as documented: the
// agent cannot tell it from a dead override.
func TestNew_OldMethodShapes(t *testing.T) {
	inner := func() agent.Tool { return idempotentLookup() }
	for _, c := range []struct {
		name    string
		tool    agent.Tool
		refused bool
	}{
		{"pointer receiver, as a pointer", &ptrRecv{inner()}, true},
		{"pointer receiver, as a value", ptrRecv{inner()}, true},
		{"nested embedding", outer{middle{inner()}}, true},
		{"declared by an embedded mixin", mixed{Tool: inner()}, true},
		{"declared by two embedded fields", ambiguous{Tool: inner()}, true},
		{"another signature", otherSig{inner()}, true},
		{"Name meaning something else", displayNamed{inner()}, true},
		{"Spec says what Safety says", legit{inner()}, false},
		{"ArgsSchema reformatted", reformatted{inner()}, false},
	} {
		_, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(), agent.WithTools(c.tool))
		if refused := errors.Is(err, agent.ErrConfig); refused != c.refused {
			t.Errorf("%s: New = %v; want refused %v", c.name, err, c.refused)
		}
	}
}

// sealingV is the sealing wrapper with Insert declared on the VALUE receiver. Passed by pointer
// (the usual way), (*sealingV).Insert is a compiler-generated wrapper.
type sealingV struct {
	*agent.MemStore
	inserts *atomic.Int64
}

func (s sealingV) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	s.inserts.Add(1)
	return s.MemStore.Insert(ctx, runID, name, data)
}

// sealingG is the same wrapper as a generic type, pointer receiver.
type sealingG[T any] struct {
	*agent.MemStore
	inserts atomic.Int64
	_       T
}

func (s *sealingG[T]) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	s.inserts.Add(1)
	return s.MemStore.Insert(ctx, runID, name, data)
}

// nested embeds the sealing wrapper, declaring nothing itself: its Insert comes from sealing.
type nested struct{ *sealing }

// A Journal over a wrapper that embeds a store and overrides Insert writes through that Insert,
// whatever the wrapper's shape: a value receiver, a generic type, or a wrapper nesting one.
func TestEmbeddingWrapper_ShapesWriteThroughTheirInsert(t *testing.T) {
	v := &sealingV{MemStore: agent.NewMemStore(), inserts: new(atomic.Int64)}
	g := &sealingG[int]{MemStore: agent.NewMemStore()}
	n := &nested{&sealing{MemStore: agent.NewMemStore()}}
	for name, tc := range map[string]struct {
		store   agent.Store
		inserts func() int64
	}{
		"value receiver by pointer": {v, v.inserts.Load},
		"generic":                   {g, g.inserts.Load},
		"nested":                    {n, n.inserts.Load},
	} {
		_, err := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), agenttest.MustJournal(tc.store)).Run(context.Background(), "r", agent.UserText("hi"))
		if err != nil || tc.inserts() == 0 {
			t.Errorf("%s: Run = %v with %d wrapper Inserts; want nil and every write through the wrapper's Insert", name, err, tc.inserts())
		}
	}
}

// Stores and the plain wrappers of the engine's tests run under a Journal.
func TestEmbeddingWrapper_PlainShapesAreAccepted(t *testing.T) {
	type countsRuns struct{ *agent.MemStore }
	for name, s := range map[string]agent.Store{
		"MemStore":        agent.NewMemStore(),
		"embeds MemStore": &countsRuns{agent.NewMemStore()},
	} {
		if _, err := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), agenttest.MustJournal(s)).Run(context.Background(), "r", agent.UserText("hi")); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// sealing is a port-level wrapper that embeds *MemStore to inherit Lister and Leaser, and
// overrides Insert (to encrypt, tenant-scope, or audit what is stored).
type sealing struct {
	*agent.MemStore
	inserts atomic.Int64
}

func (s *sealing) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	s.inserts.Add(1)
	return s.MemStore.Insert(ctx, runID, name, data)
}

// Wrapped in a Journal, such a wrapper has every write go through its Insert.
func TestEmbeddingWrapper_JournalWritesThroughItsInsert(t *testing.T) {
	ctx := context.Background()
	w := &sealing{MemStore: agent.NewMemStore()}
	j, err := agent.NewJournal(w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), j).Run(ctx, "r", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if w.inserts.Load() == 0 {
		t.Fatal("a Journal over the wrapper wrote past its Insert")
	}
}
