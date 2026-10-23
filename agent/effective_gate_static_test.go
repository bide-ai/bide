package agent

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The engine decides a call's gates in one place, effectiveGates (effective_gate.go;
// docs/design/untrusted-content-and-amount-caps.md, section 5.3). A site that reads a tool spec's
// Approval itself would skip any gate the function adds later: the pre-execution re-check, the
// snapshot a result record carries. This test type-checks every non-test file of agent, plan and
// internal/journalhook and fails on a read of ToolSpec.Approval (also through an alias, an embedding
// or a package-level function literal) outside the functions listed here. It never skips under
// -short: it is the guard that keeps a gate added to effectiveGates from being bypassed. A read of a Record's
// Approval (a recorded result) is always allowed.

// specApprovalReaders are the functions allowed to read ToolSpec.Approval, by package and name. The
// agent ones copy or validate a spec at registration or hand a copy to a middleware; none decides a
// call. The plan ones copy the tool's gate into a flow node.
var specApprovalReaders = map[string]bool{
	"agent.toolCallFor":    true, // the spec copy a middleware sees
	"agent.Spec":           true, // funcTool and subAgentTool copy their spec
	"agent.specOf":         true, // the registered copy
	"agent.WithApproval":   true, // sets the spec's gate
	"agent.addTool":        true, // registration validation
	"agent.checkEmbedded":  true, // registration validation
	"agent.finish":         true, // registration validation (m-of-n needs verifiers)
	"agent.effectiveGates": true, // the one function
	"plan.Tool":            true, // plan, out of scope until section 5.9: copies the tool's gate into the node
	"plan.RegisterTool":    true, // plan, out of scope until section 5.9: copies the tool's gate into the block
}

// approvalReads returns a description of each read of ToolSpec.Approval in the type-checked files of
// package pkg that is not in specApprovalReaders.
func approvalReads(fset *token.FileSet, pkg string, files []*ast.File, info *types.Info) []string {
	var out []string
	for _, f := range files {
		for _, d := range f.Decls {
			encl := ""
			if fn, ok := d.(*ast.FuncDecl); ok {
				encl = pkg + "." + fn.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Approval" {
					return true
				}
				s, ok := info.Selections[sel]
				if !ok || s.Kind() != types.FieldVal || fieldOwner(s) != "ToolSpec" || specApprovalReaders[encl] {
					return true
				}
				where := encl
				if where == "" {
					where = "a package-level declaration"
				}
				out = append(out, fmt.Sprintf("%s: %s reads ToolSpec.Approval directly; go through effectiveGates", fset.Position(sel.Pos()), where))
				return true
			})
		}
	}
	return out
}

// fieldOwner is the name of the struct type that declares the field s selects, following embeddings.
func fieldOwner(s *types.Selection) string {
	t := s.Recv()
	idx := s.Index()
	var name string
	for i := range idx {
		if p, ok := t.Underlying().(*types.Pointer); ok {
			t = p.Elem()
		}
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		if n, ok := t.(*types.Named); ok {
			name = n.Obj().Name()
		}
		st, ok := t.Underlying().(*types.Struct)
		if !ok {
			return ""
		}
		if i < len(idx)-1 {
			t = st.Field(idx[i]).Type()
		}
	}
	return name
}

// checkSource type-checks files (parsed from src, one package) and returns their approvalReads.
func checkSource(t *testing.T, imp types.Importer, pkg string, src map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	for name, text := range src {
		f, err := parser.ParseFile(fset, name, text, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return typeCheck(t, fset, imp, pkg, files)
}

func typeCheck(t *testing.T, fset *token.FileSet, imp types.Importer, pkg string, files []*ast.File) []string {
	t.Helper()
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: imp, Error: func(err error) { t.Errorf("type-check %s: %v", pkg, err) }}
	_, _ = conf.Check(pkg, fset, files, info)
	return approvalReads(fset, pkg, files, info)
}

func TestNoDirectApprovalReads_InTheEngine(t *testing.T) {
	imp := importer.ForCompiler(token.NewFileSet(), "source", nil)
	for pkg, dir := range map[string]string{"agent": ".", "plan": "../plan", "journalhook": "../internal/journalhook"} {
		fset := token.NewFileSet()
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var files []*ast.File
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		for _, v := range typeCheck(t, fset, imp, pkg, files) {
			t.Error(v)
		}
	}
}

// The three ways the first version of this test was evaded, and two more, are each a violation; a read
// of a recorded result is not.
func TestNoDirectApprovalReads_Fixtures(t *testing.T) {
	imp := importer.ForCompiler(token.NewFileSet(), "source", nil)
	const head = "package fixture\nimport \"github.com/bide-ai/bide/agent\"\n"
	bad := map[string]string{
		"a read in a new file":                 "func decide(s *agent.ToolSpec) *agent.ApprovalPolicy { return s.Approval }",
		"an alias that is not named s or spec": "func f(spec *agent.ToolSpec) { r := spec; _ = r.Approval }",
		"a package-level function literal":     "var g = func(s agent.ToolSpec) any { return s.Approval }",
		"a promoted field":                     "type wrap struct{ agent.ToolSpec }\nfunc h(w wrap) any { return w.Approval }",
		"an exempt name in another package":    "func toolCallFor(s agent.ToolSpec) any { return s.Approval }",
	}
	for name, body := range bad {
		if got := checkSource(t, imp, "fixture", map[string]string{"x.go": head + body}); len(got) != 1 {
			t.Errorf("%s: %d findings %v, want 1", name, len(got), got)
		}
	}
	good := head + "func rec(r agent.Record) any { return r.Approval }\nfunc rec2(r *agent.Record) any { x := r; return x.Approval }"
	if got := checkSource(t, imp, "fixture", map[string]string{"x.go": good}); len(got) != 0 {
		t.Errorf("a read of a record's Approval was flagged: %v", got)
	}
}
