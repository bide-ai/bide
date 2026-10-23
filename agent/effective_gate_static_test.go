package agent

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The engine decides a call's gates in one place, effectiveGates (effective_gate.go;
// docs/design/untrusted-content-and-amount-caps.md, section 5.3). A site that reads a tool spec's
// Approval itself would skip any gate the function adds later: the pre-execution re-check, the
// snapshot a result record carries. This test type-checks every non-test file of agent, plan and
// internal/journalhook and fails on any selector that resolves to the field object ToolSpec.Approval,
// whatever it is reached through (an alias, a defined type converted from a ToolSpec, an embedding, a
// package-level function literal), and on reflect's field lookup by name (FieldByName on a reflect.Type
// or reflect.Value or a type embedding one, called with "Approval" or a name the test cannot read,
// FieldByNameFunc, or taken as a method value), outside the functions
// listed here. It never skips under -short: it is the guard that keeps a gate added to effectiveGates
// from being bypassed. Residual and out of scope: Field(i) by index, encoding/json, unsafe, and code outside agent, plan and internal/journalhook. A read of a Record's Approval
// (a recorded result) is a different field and always allowed.

const agentPath = "github.com/bide-ai/bide/agent"

// specApprovalReaders are the functions allowed to read ToolSpec.Approval, keyed by package, receiver
// and name, so a later function that merely shares a name is not exempt. The agent ones copy or
// validate a spec at registration or hand a copy to a middleware; none decides a call. The plan ones
// copy the tool's gate into a flow node.
var specApprovalReaders = map[string]bool{
	"agent.(*Agent).toolCallFor":    true, // the spec copy a middleware sees
	"agent.(*funcTool).Spec":        true, // a func tool copies its spec
	"agent.(*subAgentTool).Spec":    true, // a sub-agent tool copies its spec
	"agent.specOf":                  true, // the registered copy
	"agent.WithApproval":            true, // sets the spec's gate
	"agent.(*Agent).addTool":        true, // registration validation
	"agent.checkEmbedded":           true, // registration validation
	"agent.(*agentConfig).finish":   true, // registration validation (m-of-n needs verifiers)
	"agent.effectiveGates":          true, // the one function
	"plan.(*Builder).Tool":          true, // plan, out of scope until section 5.9: copies the tool's gate into the node
	"plan.(*Registry).RegisterTool": true, // plan, out of scope until section 5.9: copies the tool's gate into the block
}

var typeArgs = regexp.MustCompile(`\[[^\]]*\]`)

// funcKey is the key of fn in specApprovalReaders: package, then "(*T)." or "(T)." for a method, then
// the name.
func funcKey(pkg string, fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return pkg + "." + fn.Name.Name
	}
	return pkg + ".(" + typeArgs.ReplaceAllString(types.ExprString(fn.Recv.List[0].Type), "") + ")." + fn.Name.Name
}

// approvalReads returns a description of each read of ToolSpec.Approval, or reflective lookup of a
// field named Approval, in files (one type-checked package labelled pkg) outside specApprovalReaders.
// approval is the field object ToolSpec.Approval.
func approvalReads(fset *token.FileSet, pkg string, files []*ast.File, info *types.Info, approval *types.Var) []string {
	var out []string
	for _, f := range files {
		for _, d := range f.Decls {
			encl := ""
			if fn, ok := d.(*ast.FuncDecl); ok {
				encl = funcKey(pkg, fn)
			}
			report := func(n ast.Node, what string) {
				where := encl
				if where == "" {
					where = "a package-level declaration"
				}
				out = append(out, fmt.Sprintf("%s: %s %s; go through effectiveGates", fset.Position(n.Pos()), where, what))
			}
			called := map[*ast.SelectorExpr]bool{} // lookups that are the function of a call, judged with it
			ast.Inspect(d, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if s, ok := info.Selections[n]; ok && s.Kind() == types.FieldVal && s.Obj() == approval && !specApprovalReaders[encl] {
						report(n, "reads ToolSpec.Approval directly")
					}
					if !called[n] && reflectFieldLookup(n, info) && !specApprovalReaders[encl] {
						report(n, "takes reflect's field lookup as a value")
					}
				case *ast.CallExpr:
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok && reflectFieldLookup(sel, info) {
						called[sel] = true
						if !safeReflectCall(n, sel, info) && !specApprovalReaders[encl] {
							report(n, "looks up a field by reflection with a name that is, or may be, Approval")
						}
					}
				}
				return true
			})
		}
	}
	return out
}

// reflectFieldLookup reports whether sel selects reflect's FieldByName or FieldByNameFunc method, on
// reflect.Type or reflect.Value or on a type that embeds either.
func reflectFieldLookup(sel *ast.SelectorExpr, info *types.Info) bool {
	if sel.Sel.Name != "FieldByName" && sel.Sel.Name != "FieldByNameFunc" {
		return false
	}
	s, ok := info.Selections[sel]
	if !ok {
		return false
	}
	fn, ok := s.Obj().(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == "reflect"
}

// safeReflectCall reports whether call is FieldByName("x") with a constant name other than
// "Approval": the only use of the lookup the test lets through. Anything else is flagged: the name
// "Approval", a name it cannot read, FieldByNameFunc, a method value (fn := v.FieldByName).
func safeReflectCall(call *ast.CallExpr, sel *ast.SelectorExpr, info *types.Info) bool {
	if sel.Sel.Name != "FieldByName" || len(call.Args) != 1 {
		return false
	}
	tv := info.Types[call.Args[0]]
	return tv.Value != nil && tv.Value.Kind() == constant.String && constant.StringVal(tv.Value) != "Approval"
}

// approvalField is ToolSpec's Approval field of the agent package pkg.
func approvalField(t *testing.T, pkg *types.Package) *types.Var {
	t.Helper()
	obj := pkg.Scope().Lookup("ToolSpec")
	if obj == nil {
		t.Fatal("no ToolSpec in the agent package")
	}
	st, ok := obj.Type().Underlying().(*types.Struct)
	if !ok {
		t.Fatal("ToolSpec is not a struct")
	}
	for i := range st.NumFields() {
		if st.Field(i).Name() == "Approval" {
			return st.Field(i)
		}
	}
	t.Fatal("ToolSpec has no Approval field")
	return nil
}

// typeCheck type-checks files as package pkg (the label used in specApprovalReaders); isAgent says
// they are the agent package itself, whose ToolSpec the checked package defines rather than imports.
func typeCheck(t *testing.T, fset *token.FileSet, imp types.Importer, pkg string, isAgent bool, files []*ast.File) []string {
	t.Helper()
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: imp, Error: func(err error) { t.Errorf("type-check %s: %v", pkg, err) }}
	checked, _ := conf.Check(pkg, fset, files, info)
	agentPkg := checked
	if !isAgent {
		var err error
		if agentPkg, err = imp.Import(agentPath); err != nil {
			t.Fatal(err)
		}
	}
	return approvalReads(fset, pkg, files, info, approvalField(t, agentPkg))
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
		for _, v := range typeCheck(t, fset, imp, pkg, pkg == "agent", files) {
			t.Error(v)
		}
	}
}

// The ways the test has been evaded are each a violation, and a read of a recorded result is not.
func TestNoDirectApprovalReads_Fixtures(t *testing.T) {
	imp := importer.ForCompiler(token.NewFileSet(), "source", nil)
	check := func(label, body string) []string {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", "package fixture\nimport (\n\t\"reflect\"\n\t\"github.com/bide-ai/bide/agent\"\n)\nvar _ = reflect.TypeOf\n"+body, 0)
		if err != nil {
			t.Fatal(err)
		}
		return typeCheck(t, fset, imp, label, false, []*ast.File{f})
	}
	bad := map[string]string{
		"a read in a new file":                 "func decide(s *agent.ToolSpec) *agent.ApprovalPolicy { return s.Approval }",
		"an alias variable":                    "func f(spec *agent.ToolSpec) { r := spec; _ = r.Approval }",
		"a package-level function literal":     "var g = func(s agent.ToolSpec) any { return s.Approval }",
		"a promoted field":                     "type wrap struct{ agent.ToolSpec }\nfunc h(w wrap) any { return w.Approval }",
		"a type alias":                         "type ts = agent.ToolSpec\nfunc a(s ts) any { return s.Approval }",
		"a defined type converted from a spec": "type sc agent.ToolSpec\nfunc c(s agent.ToolSpec) any { return sc(s).Approval }",
		"reflect FieldByName on a type":        "func r1() { reflect.TypeOf(agent.ToolSpec{}).FieldByName(\"Approval\") }",
		"reflect FieldByName on a value":       "func r2(s agent.ToolSpec) { reflect.ValueOf(s).FieldByName(\"Approval\") }",
		"an exempt name on another receiver":   "type other struct{}\nfunc (o *other) Spec(s agent.ToolSpec) any { return s.Approval }",
		"an exempt name in another package":    "func specOf(s agent.ToolSpec) any { return s.Approval }",
	}
	for name, body := range bad {
		if got := check("fixture", body); len(got) != 1 {
			t.Errorf("%s: %d findings %v, want 1", name, len(got), got)
		}
	}
	// A method named Spec on the exempt receiver in the exempt package is the only way through: a fixture
	// labelled as the agent package with a funcTool-shaped receiver is exempt, one with another is not.
	if got := check("agent", "type funcTool[In, Out any] struct{}\nfunc (t *funcTool[In, Out]) Spec(s agent.ToolSpec) any { return s.Approval }"); len(got) != 0 {
		t.Errorf("the exempt (*funcTool).Spec was flagged: %v", got)
	}
	good := "func rec(r agent.Record) any { return r.Approval }\nfunc rec2(r *agent.Record) any { x := r; return x.Approval }\nfunc nm() { reflect.TypeOf(agent.Record{}).FieldByName(\"Safety\") }"
	if got := check("fixture", good); len(got) != 0 {
		t.Errorf("a read of a record's Approval, or another field by reflection, was flagged: %v", got)
	}
}
