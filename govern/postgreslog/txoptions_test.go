package postgreslog

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every transaction the log begins sets its own isolation, read committed, and no statement that
// changes data runs outside one, so no transaction inherits the deployment's
// default_transaction_isolation (see txOptions). The behavioural tests catch the writes whose
// outcome depends on the level; this check also holds the ones whose outcome does not (the schema
// migration), so a later change cannot reintroduce the default for any of them unnoticed.
func TestEveryTransactionSetsItsIsolation(t *testing.T) {
	if txOptions == nil || txOptions.Isolation != sql.LevelReadCommitted {
		t.Fatalf("txOptions = %+v, want read committed", txOptions)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	begins := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "BeginTx":
				begins++
				if len(call.Args) != 2 {
					t.Errorf("%s: BeginTx with %d arguments", fset.Position(call.Pos()), len(call.Args))
				} else if id, ok := call.Args[1].(*ast.Ident); !ok || id.Name != "txOptions" {
					t.Errorf("%s: BeginTx without txOptions inherits the deployment's default isolation", fset.Position(call.Pos()))
				}
			case "Begin", "Exec", "ExecContext":
				if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "db" {
					t.Errorf("%s: %s on the pool runs at the deployment's default isolation; use a transaction begun with txOptions", fset.Position(call.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	if begins == 0 {
		t.Fatal("found no BeginTx calls; the check is not looking at the package's source")
	}
}
