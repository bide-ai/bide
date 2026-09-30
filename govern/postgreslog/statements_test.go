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
	"regexp"
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
// The same methods called through an interface or a method expression are refused, since the check
// cannot tell which handle they reach, and nothing may assign to txOptions or through it. Beyond
// the pool (see checkExpr): no function of a pgx package may be used outside migrate, no constant
// names a session-level advisory lock, and a constant query calls no function outside
// constantCalls, but for the append's call of nextSeqFunction. TestStatementCheckCatchesBypasses holds the check
// to a fixture of ways around it.
func TestStatementsOnThePool(t *testing.T) {
	if txOptions == nil || txOptions.Isolation != sql.LevelReadCommitted || txOptions.ReadOnly {
		t.Fatalf("txOptions = %+v, want read committed", txOptions)
	}
	for _, problem := range statementProblems(t, ".") {
		t.Error(problem)
	}
}

// The check reports every hole in testdata/bypass.go.txt, a file of ways to hold a transaction or
// a lock across round trips, type-checked with the package's files: each line marked BYPASS must
// be reported, and no other line of the file.
func TestStatementCheckCatchesBypasses(t *testing.T) {
	const fixture = "testdata/bypass.go.txt"
	src, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	reported := map[int][]string{}
	for _, p := range statementProblems(t, ".", fixture) {
		file, rest, _ := strings.Cut(p, ":")
		if filepath.Base(file) != filepath.Base(fixture) {
			t.Errorf("a problem outside the fixture: %s", p)
			continue
		}
		line, _, _ := strings.Cut(rest, ":")
		var n int
		fmt.Sscan(line, &n)
		reported[n] = append(reported[n], p)
	}
	marked := 0
	for i, line := range strings.Split(string(src), "\n") {
		n := i + 1
		_, what, ok := strings.Cut(line, "// BYPASS: ")
		if !ok {
			for _, p := range reported[n] {
				t.Errorf("reported an unmarked line: %s", p)
			}
			continue
		}
		marked++
		if len(reported[n]) == 0 {
			t.Errorf("%s:%d not reported: %s", fixture, n, what)
		}
	}
	if marked == 0 {
		t.Fatal("the fixture marks no line")
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
func statementProblems(t *testing.T, dir string, extra ...string) []string {
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
	for _, name := range append(names, extra...) {
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
				if e, ok := n.(ast.Expr); ok {
					checkExpr(e, fnName, info, report)
				}
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
					if sel == nil || sel.Kind() == types.FieldVal {
						return true
					}
					idx, governed := poolMethods[n.Sel.Name]
					if !governed {
						return true
					}
					fn := sel.Obj().(*types.Func)
					recv := fn.Signature().Recv().Type()
					if sel.Kind() == types.MethodExpr {
						if types.IsInterface(recv) || isPoolHandle(fn, recv) {
							report(n, "%s through a method expression, which hides the call from the check", n.Sel.Name)
						}
						return true
					}
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
						} else if name, ok := unknownCall(q); ok {
							report(call.Args[idx], "%s on %s calls %s, a function the check does not know", n.Sel.Name, recv, name)
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

// sessionLockCall matches the session-level advisory lock functions, whose lock outlives the
// statement and so is held across round trips. The transaction-level forms (pg_advisory_xact_lock
// and pg_try_advisory_xact_lock) end with the statement's transaction.
var sessionLockCall = regexp.MustCompile(`(?i)pg_(try_)?advisory_lock`)

// constantCalls are the only functions a constant query on the pool may call, each qualified with
// pg_catalog; nextSeqFunction only in Append's INSERT, once, with no SELECT. A name after INTO
// names a table, and a keyword in listWords takes a list; neither is a call.
var (
	constantCalls = map[string]bool{"pg_catalog.unnest": true, "pg_catalog.array_agg": true,
		"pg_catalog.current_schema": true, "pg_catalog.current_schemas": true, "pg_catalog.array_position": true}
	listWords = map[string]bool{"values": true, "conflict": true, "exists": true, "in": true, "any": true, "as": true, "and": true, "or": true, "not": true, "on": true, "coalesce": true}
	quotedSQL = regexp.MustCompile(`'(?:[^']|'')*'|"(?:[^"]|"")*"`)
	callSQL   = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_$.]*)\s*\(`)
	// Spellings a scan of the text cannot follow: a Unicode escape, an escape string, a
	// backslash, a comment, a dollar quote; and a quoted identifier used as a function or schema
	// name. The log's own queries use none of them.
	hiddenSQL     = regexp.MustCompile(`(?i)u&["']|(^|[^a-z0-9_$])e'|\\|--|/\*|\$([a-z_][a-z0-9_]*)?\$`)
	quotedNameSQL = regexp.MustCompile(`"(?:[^"]|"")*"\s*[(.]`)
	selectSQLWord = regexp.MustCompile(`(?i)(^|[^a-z0-9_$])select([^a-z0-9_$]|$)`)
)

// unknownCall returns why constant query q may not run on the pool: a spelling that hides a call
// from the scan, or a call to a function outside constantCalls (unqualified, one a deployment
// defined, or nextSeqFunction outside a one-row INSERT or more than once).
func unknownCall(q string) (string, bool) {
	if m := hiddenSQL.FindString(q); m != "" {
		return fmt.Sprintf("%q, which hides SQL from the check,", m), true
	}
	literals := quotedSQL.ReplaceAllStringFunc(q, func(lit string) string {
		if lit[0] == '"' {
			return lit
		}
		return "''"
	})
	if m := quotedNameSQL.FindString(literals); m != "" {
		return fmt.Sprintf("the quoted name %s", m), true
	}
	q = quotedSQL.ReplaceAllString(literals, "''")
	nextSeqOK := isKeyword(q, "INSERT") && !selectSQLWord.MatchString(q)
	for _, m := range callSQL.FindAllStringSubmatchIndex(q, -1) {
		name := strings.ToLower(q[m[2]:m[3]])
		if before := strings.Fields(q[:m[2]]); len(before) > 0 && strings.EqualFold(before[len(before)-1], "INTO") {
			continue
		}
		switch {
		case listWords[name] || constantCalls[name]:
		case name == nextSeqFunction && nextSeqOK:
			nextSeqOK = false // once
		default:
			return name, true
		}
	}
	return "", false
}

// pgxPackage reports whether path is one of the pgx packages, whose connections and transactions
// the database/sql rules do not see.
func pgxPackage(path string) bool {
	return path == "github.com/jackc/pgx/v5" || strings.HasPrefix(path, "github.com/jackc/pgx/v5/")
}

// checkExpr applies the rules that hold for any expression in the function fnName (empty outside
// a function): a function or method of a pgx package may be used only in migrate, since the log
// reaches the database through database/sql and a pgx connection or transaction would escape
// every rule; and no constant string may name a session-level advisory lock. A constant
// identifier is reported where the constant is declared, not where it is used.
func checkExpr(e ast.Expr, fnName string, info *types.Info, report func(ast.Node, string, ...any)) {
	if id, ok := e.(*ast.Ident); ok {
		if fn, ok := info.Uses[id].(*types.Func); ok && fn.Pkg() != nil && pgxPackage(fn.Pkg().Path()) && fnName != "migrate" {
			report(id, "%s of %s outside migrate: a pgx connection or transaction escapes the database/sql rules", id.Name, fn.Pkg().Path())
		}
		return
	}
	if tv, ok := info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.String && sessionLockCall.MatchString(constant.StringVal(tv.Value)) {
		report(e, "a session-level advisory lock, held across round trips")
	}
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
