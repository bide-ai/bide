package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// TestNoDirectApprovalReads_InTheEngine: the engine's call path decides a call's gates in one place,
// effectiveGates (effective_gate.go; docs/design/untrusted-content-and-amount-caps.md, section 5.3).
// A site that reads a tool spec's Approval itself would skip any gate the function adds later (the
// pre-execution re-check, the snapshot a result record carries). In loop.go, toolexec.go and saga.go
// the only other reads of a field named Approval allowed are a recorded result's (receiver r, copying
// a record to its saga_fail twin) and toolCallFor's copy of the spec for a middleware.
func TestNoDirectApprovalReads_InTheEngine(t *testing.T) {
	fset := token.NewFileSet()
	for _, file := range []string{"loop.go", "toolexec.go", "saga.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Approval" {
					return true
				}
				recv := types.ExprString(sel.X)
				if recv == "r" || fn.Name.Name == "toolCallFor" {
					return true
				}
				t.Errorf("%s: %s reads %s.Approval directly; go through effectiveGates", fset.Position(sel.Pos()), fn.Name.Name, recv)
				return true
			})
		}
	}
}
