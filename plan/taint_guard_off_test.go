package plan

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// taintGuardOff and taintInheritOn are the test-only switches of flow taint: the first turns off the refusal of
// a flow that declares taint, the second turns on the inheritance of taint by a nested flow. Both must be
// impossible to reach outside a test: no build tag, environment variable, option or config key sets them, and
// no file that is not a _test.go file assigns them, takes their address, or reads them except the one function
// that implements each (taintNotEnforced, inheritsTaint). The change that completes the enforcement removes
// the switches and this test with them.
func TestTaintSwitchesAreAssignedOnlyInTests(t *testing.T) {
	readers := map[string]string{"taintGuardOff": "taintNotEnforced", "taintInheritOn": "inheritsTaint"}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	uses := map[string]int{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, isFn := d.(*ast.FuncDecl)
			_, isGen := d.(*ast.GenDecl)
			ast.Inspect(d, func(x ast.Node) bool {
				switch x := x.(type) {
				case *ast.AssignStmt:
					for _, l := range x.Lhs {
						if id, ok := l.(*ast.Ident); ok && readers[id.Name] != "" {
							t.Errorf("%s: assigns %s outside a test", fset.Position(x.Pos()), id.Name)
						}
					}
				case *ast.IncDecStmt:
					if id, ok := x.X.(*ast.Ident); ok && readers[id.Name] != "" {
						t.Errorf("%s: changes %s outside a test", fset.Position(x.Pos()), id.Name)
					}
				case *ast.UnaryExpr:
					if id, ok := x.X.(*ast.Ident); ok && readers[id.Name] != "" && x.Op == token.AND {
						t.Errorf("%s: takes the address of %s outside a test", fset.Position(x.Pos()), id.Name)
					}
				case *ast.Ident:
					want, ok := readers[x.Name]
					if !ok {
						return true
					}
					uses[x.Name]++
					if !isGen && (!isFn || fn.Name.Name != want) {
						t.Errorf("%s: reads %s outside %s", fset.Position(x.Pos()), x.Name, want)
					}
				}
				return true
			})
		}
	}
	for name := range readers {
		if uses[name] < 2 {
			t.Fatalf("found %d uses of %s in non-test files; the scan is not seeing it", uses[name], name)
		}
	}
}
