package postgreslog

import (
	"database/sql"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// No transaction the log begins outside the schema migration spans round trips: an append is one
// INSERT sent on the pool, which Postgres runs as a transaction of its own (see the package
// documentation), so a process stalled between two round trips holds no lock. The migration, the
// one transaction of several statements, sets its own isolation, read committed, so it does not
// inherit the deployment's default_transaction_isolation (see txOptions). The stall and isolation
// tests check the behaviour; this check holds every statement a later change adds to the rules.
//
// The check type-checks the package's source, so it follows types, not names: every method of
// *sql.DB or *sql.Conn that runs SQL or begins a transaction is found whatever its receiver is
// called or however it is reached (a field, a parameter, a local, an embedded field). On those:
//   - BeginTx may be called only in the method migrate, and must pass the package-level txOptions
//     (a local of the same name does not count);
//   - Exec, Query and QueryRow (and their Context forms) must pass a constant query that is one
//     statement (it holds no semicolon) and starts with SELECT or INSERT;
//   - Begin, Prepare and Raw, which could run anything, and a method value that escapes the check,
//     are refused.
//
// The same methods called through an interface are refused, since the check cannot tell which
// handle they reach; and nothing may assign to txOptions or through it.
func TestStatementsOnThePool(t *testing.T) {
	if txOptions == nil || txOptions.Isolation != sql.LevelReadCommitted || txOptions.ReadOnly {
		t.Fatalf("txOptions = %+v, want read committed", txOptions)
	}
	for _, problem := range statementProblems(t, ".") {
		t.Error(problem)
	}
}

// poolMethods are the methods of *sql.DB and *sql.Conn the statement check governs, with the
// index of the query argument, or -1 for a method refused outright.
var poolMethods = map[string]int{
	"Exec": 0, "ExecContext": 1,
	"Query": 0, "QueryContext": 1,
	"QueryRow": 0, "QueryRowContext": 1,
	"BeginTx": -2,
	"Begin":   -1, "Prepare": -1, "PrepareContext": -1, "Raw": -1,
}

// statementProblems type-checks the non-test Go files in dir and returns every violation of the
// rules TestStatementsOnThePool states, each with its position.
func statementProblems(t *testing.T, dir string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	// Export data for every dependency, from the build cache, so the importer sees the same
	// packages the build does.
	cmd := exec.Command("go", "list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}", ".")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -export: %v", err)
	}
	exports := map[string]string{}
	for line := range strings.Lines(string(out)) {
		path, file, _ := strings.Cut(strings.TrimSpace(line), "=")
		exports[path] = file
	}
	fset := token.NewFileSet()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, os.ErrNotExist
		}
		return os.Open(file)
	})
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	pkg, err := (&types.Config{Importer: imp}).Check(files[0].Name.Name, fset, files, info)
	if err != nil {
		t.Fatalf("type-check: %v", err)
	}
	opts := pkg.Scope().Lookup("txOptions")
	if opts == nil {
		t.Fatal("the package declares no txOptions")
	}

	var problems []string
	report := func(n ast.Node, format string, args ...any) {
		problems = append(problems, fset.Position(n.Pos()).String()+": "+fmt.Sprintf(format, args...))
	}
	calls := map[*ast.SelectorExpr]*ast.CallExpr{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
					calls[sel] = call
				}
			}
			return true
		})
	}
	begins, reads, writes := 0, 0, 0
	for _, f := range files {
		for _, decl := range f.Decls {
			fnName := ""
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fnName = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if id := rootIdent(lhs); id != nil && info.Uses[id] == opts {
							report(lhs, "assigns to txOptions")
						}
					}
				case *ast.IncDecStmt:
					if id := rootIdent(n.X); id != nil && info.Uses[id] == opts {
						report(n, "assigns to txOptions")
					}
				case *ast.UnaryExpr:
					if id := rootIdent(n.X); n.Op == token.AND && id != nil && info.Uses[id] == opts {
						report(n, "takes the address of txOptions or a field of it")
					}
				case *ast.SelectorExpr:
					sel := info.Selections[n]
					if sel == nil || sel.Kind() != types.MethodVal {
						return true
					}
					idx, governed := poolMethods[n.Sel.Name]
					if !governed {
						return true
					}
					fn := sel.Obj().(*types.Func)
					recv := fn.Signature().Recv().Type()
					if types.IsInterface(recv) {
						report(n, "%s through an interface: the check cannot tell whether it reaches the pool", n.Sel.Name)
						return true
					}
					if !isPoolHandle(fn, recv) {
						return true
					}
					call := calls[n]
					if call == nil {
						report(n, "%s of %s used as a value, outside a call the check can see", n.Sel.Name, recv)
						return true
					}
					switch {
					case idx == -1:
						report(n, "%s on %s can run SQL outside a transaction begun with txOptions", n.Sel.Name, recv)
					case idx == -2:
						begins++
						if fnName != "migrate" {
							report(n, "BeginTx on %s outside migrate: a transaction the log begins must not span round trips", recv)
						}
						if id, ok := ast.Unparen(call.Args[1]).(*ast.Ident); !ok || info.Uses[id] != opts {
							report(call.Args[1], "BeginTx on %s without the package-level txOptions inherits the deployment's default isolation", recv)
						}
					default:
						tv := info.Types[call.Args[idx]]
						if tv.Value == nil || tv.Value.Kind() != constant.String {
							report(call.Args[idx], "%s on %s with a query that is not a constant string", n.Sel.Name, recv)
						} else if q := constant.StringVal(tv.Value); strings.Contains(q, ";") {
							report(call.Args[idx], "%s on %s runs more than one statement", n.Sel.Name, recv)
						} else if isKeyword(q, "SELECT") {
							reads++
						} else if isKeyword(q, "INSERT") {
							writes++
						} else {
							report(call.Args[idx], "%s on %s runs a statement other than SELECT or INSERT", n.Sel.Name, recv)
						}
					}
				}
				return true
			})
		}
	}
	if begins == 0 || reads == 0 || writes == 0 {
		t.Fatalf("found %d BeginTx calls, %d reads and %d writes on the pool; the check is not seeing the package's calls", begins, reads, writes)
	}
	return problems
}

// isPoolHandle reports whether fn is a method of *sql.DB or *sql.Conn, whose statements run
// outside any transaction the caller began.
func isPoolHandle(fn *types.Func, recv types.Type) bool {
	if fn.Pkg() == nil || fn.Pkg().Path() != "database/sql" {
		return false
	}
	ptr, ok := recv.(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	return ok && (named.Obj().Name() == "DB" || named.Obj().Name() == "Conn")
}

// isKeyword reports whether query starts with the keyword kw, after leading white space.
func isKeyword(query, kw string) bool {
	q := strings.TrimLeftFunc(query, unicode.IsSpace)
	return len(q) > len(kw) && strings.EqualFold(q[:len(kw)], kw) && unicode.IsSpace(rune(q[len(kw)]))
}

// rootIdent returns the identifier an expression such as x, x.f, x[i], *x or (x) is rooted at.
func rootIdent(e ast.Expr) *ast.Ident {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return nil
		}
	}
}
