package plan

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// taintGuardOff turns the refusal of flow taint off for the tests of its enforcement. It must be impossible to
// reach outside a test: no build tag, environment variable, option or config key sets it, and no file that is
// not a _test.go file assigns it, takes its address or reads it except the one function that implements the
// refusal.
func TestTaintGuardOffIsAssignedOnlyInTests(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	uses := 0
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
			ast.Inspect(d, func(x ast.Node) bool {
				switch x := x.(type) {
				case *ast.AssignStmt:
					for _, l := range x.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name == "taintGuardOff" {
							t.Errorf("%s: assigns taintGuardOff outside a test", fset.Position(x.Pos()))
						}
					}
				case *ast.IncDecStmt:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "taintGuardOff" {
						t.Errorf("%s: changes taintGuardOff outside a test", fset.Position(x.Pos()))
					}
				case *ast.UnaryExpr:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "taintGuardOff" && x.Op == token.AND {
						t.Errorf("%s: takes the address of taintGuardOff outside a test", fset.Position(x.Pos()))
					}
				case *ast.Ident:
					if x.Name == "taintGuardOff" {
						uses++
						if !isFn || fn.Name.Name != "taintNotEnforced" {
							// The declaration itself is a GenDecl, not a function.
							if _, gen := d.(*ast.GenDecl); !gen {
								t.Errorf("%s: reads taintGuardOff outside taintNotEnforced", fset.Position(x.Pos()))
							}
						}
					}
				}
				return true
			})
		}
	}
	if uses < 2 {
		t.Fatalf("found %d uses of taintGuardOff in non-test files; the scan is not seeing it", uses)
	}
}
