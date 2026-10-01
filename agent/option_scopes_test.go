package agent_test

import (
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
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
		"agent-only options": `
			_ = []agent.Option{agent.WithTools(), agent.WithMiddleware(), agent.WithToolMiddleware(), agent.WithApproverVerifiers(nil),
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
		"WithMaxTurns as a ParallelOption":            {`var _ agent.ParallelOption = agent.WithMaxTurns(1)`, "applyParallel"},
		"WithMaxTurns as a ResolveOption":             {`var _ agent.ResolveOption = agent.WithMaxTurns(1)`, "applyResolve"},
		"WithMaxTurns as a ToolOption":                {`var _ agent.ToolOption = agent.WithMaxTurns(1)`, "applyTool"},
		"WithMaxTurns as a StepOption":                {`var _ agent.StepOption = agent.WithMaxTurns(1)`, "applyStep"},
		"WithMaxTurns as a LeaseOption":               {`var _ agent.LeaseOption = agent.WithMaxTurns(1)`, "applyLease"},
		"WithIdentity as a ParallelOption":            {`var _ agent.ParallelOption = agent.WithIdentity(agent.Identity{})`, "applyParallel"},
		"WithTools as a RunOption":                    {`var _ agent.RunOption = agent.WithTools()`, "applyRun"},
		"WithRetrieval as a RunOption":                {`var _ agent.RunOption = agent.WithRetrieval(nil, 1)`, "applyRun"},
		"WithSystemPromptFunc as a RunOption":         {`var _ agent.RunOption = agent.WithSystemPromptFunc(nil)`, "applyRun"},
		"WithOptions as a RunOption":                  {`var _ agent.RunOption = agent.WithOptions()`, "applyRun"},
		"WithMaxConcurrency as a ResolveOption":       {`var _ agent.ResolveOption = agent.WithMaxConcurrency(1)`, "applyResolve"},
		"WithMaxConcurrency as a StepOption":          {`var _ agent.StepOption = agent.WithMaxConcurrency(1)`, "applyStep"},
		"WithClock as a ParallelOption":               {`var _ agent.ParallelOption = agent.WithClock(nil)`, "applyParallel"},
		"WithClock as a StepOption":                   {`var _ agent.StepOption = agent.WithClock(nil)`, "applyStep"},
		"WithSafety as an Option":                     {`var _ agent.Option = agent.WithSafety(agent.Safety{})`, "applyAgent"},
		"WithSafety as a RunOption":                   {`var _ agent.RunOption = agent.WithSafety(agent.Safety{})`, "applyRun"},
		"WithSafety as a ParallelOption":              {`var _ agent.ParallelOption = agent.WithSafety(agent.Safety{})`, "applyParallel"},
		"WithTimeout as a StepOption":                 {`var _ agent.StepOption = agent.WithTimeout(1)`, "applyStep"},
		"WithLeaseTTL as an Option":                   {`var _ agent.Option = agent.WithLeaseTTL(1)`, "applyAgent"},
		"WithLeaseHolder as a RunOption":              {`var _ agent.RunOption = agent.WithLeaseHolder("")`, "applyRun"},
		"WithRecoverInterval as a RecoverOption":      {`var _ agent.RecoverOption = agent.WithRecoverInterval(1)`, "applyRecover"},
		"WithRecoverErrors as a LeaseOption":          {`var _ agent.LeaseOption = agent.WithRecoverErrors(nil)`, "applyLease"},
		"WithMinHaltAge as an Option":                 {`var _ agent.Option = agent.WithMinHaltAge(1)`, "applyAgent"},
		"WithEvidence as a RunOption":                 {`var _ agent.RunOption = agent.WithEvidence(nil)`, "applyRun"},
		"WithRecoverConcurrency in Recover's options": {`_ = []agent.RecoverOption{agent.WithRecoverConcurrency(1)}`, "applyRecover"},
		"an Option in a RunOption slice":              {`_ = []agent.RunOption{agent.WithMaxTurns(1), agent.WithTools()}`, "applyRun"},
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
		"LeaseOption", "RecoverOption", "RecoverLoopOption"}
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
	if want := []string{"AgentRunOption", "ClockOption", "ConcurrencyOption", "LeaseControl", "SafetyOption"}; !slices.Equal(combos, want) {
		t.Errorf("combination types = %v, want the closed list %v", combos, want)
	}

	// Every option constructor returns its narrowest type, and a new one must be listed here.
	want := map[string]string{
		"WithMaxTurns": "AgentRunOption", "WithTokenBudget": "AgentRunOption", "WithSystemPrompt": "AgentRunOption",
		"WithSampling": "AgentRunOption", "WithToolChoice": "AgentRunOption", "WithWaker": "AgentRunOption",
		"WithIdentity":       "AgentRunOption",
		"WithMaxConcurrency": "ConcurrencyOption",
		"WithClock":          "ClockOption",
		"WithSafety":         "SafetyOption",
		"WithLeaseHolder":    "LeaseControl", "WithLeaseTTL": "LeaseControl",
		"WithTools": "Option", "WithMiddleware": "Option", "WithToolMiddleware": "Option", "WithApproverVerifiers": "Option",
		"WithToolErrorRedactor": "Option", "WithRetrieval": "Option", "WithSystemPromptFunc": "Option", "WithOptions": "Option",
		"WithRecoverInterval": "RecoverLoopOption", "WithRecoverConcurrency": "RecoverLoopOption", "WithRecoverErrors": "RecoverLoopOption",
		"WithMinHaltAge": "ResolveOption", "WithoutLiveDriverCheck": "ResolveOption", "WithEvidence": "ResolveOption",
		"WithApproval": "ToolOption", "WithTimeout": "ToolOption", "WithTitle": "ToolOption", "WithOutputSchema": "ToolOption",
	}
	optionTypes := append(slices.Clone(scopes), "AgentRunOption", "ConcurrencyOption", "ClockOption", "SafetyOption", "LeaseControl")
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
	src := "package p\n\nimport \"github.com/bide-ai/bide/agent\"\n\nvar _ = agent.Build\n\nfunc _() {\n" + body + "\n}\n"
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
