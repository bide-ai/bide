package plan

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A finding gets its kind, and a problem its place, where the problem is made, never by reading the
// problem's text back: matching on a message is the coupling that broke the editor's comparison when a
// message shortened a name or changed a position. This walks the source of plan, the editor and the
// viewer's rendering and fails on any code that matches, searches or compares a Message: a strings,
// bytes, slices or regexp call, a map lookup, ==, != or a switch, on the field directly or on a local
// that was assigned from it. A lookup by text cannot be added without changing this test on purpose.
func TestNoCodeMatchesOnAMessage(t *testing.T) {
	for _, dir := range []string{".", "../flowedit", "../internal/runview"} {
		for _, msg := range messageMatches(t, dir, nil) {
			t.Error(msg)
		}
	}
}

// allowedMessageUse lists the places that read a Message for display, not to decide anything. Each entry
// is a file and the reason; the guard skips the file.
var allowedMessageUse = map[string]string{}

// messageMatches returns the places in the non-test Go files of dir (or in src, a map of file name to
// source, when given) that match on a Message.
func messageMatches(t *testing.T, dir string, src map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	if src != nil {
		for name, text := range src {
			f, err := parser.ParseFile(fset, name, text, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
	} else {
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") || allowedMessageUse[filepath.Base(name)] != "" {
				continue
			}
			text, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(fset, name, text, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
	}
	var out []string
	report := func(n ast.Node, what string) { out = append(out, fset.Position(n.Pos()).String()+": "+what) }
	for _, file := range files {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			// The locals that hold a Message: assigned from one, or from another such local.
			tainted := map[string]bool{}
			var mentions func(ast.Node) bool
			mentions = func(n ast.Node) bool {
				found := false
				ast.Inspect(n, func(x ast.Node) bool {
					switch y := x.(type) {
					case *ast.SelectorExpr:
						if y.Sel.Name == "Message" {
							found = true
						}
					case *ast.Ident:
						if tainted[y.Name] {
							found = true
						}
					}
					return !found
				})
				return found
			}
			for changed := true; changed; {
				changed = false
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.AssignStmt:
						for i, l := range x.Lhs {
							id, ok := l.(*ast.Ident)
							if !ok || tainted[id.Name] {
								continue
							}
							rhs := x.Rhs[0]
							if len(x.Rhs) == len(x.Lhs) {
								rhs = x.Rhs[i]
							}
							if mentions(rhs) {
								tainted[id.Name] = true
								changed = true
							}
						}
					case *ast.ValueSpec:
						for i, id := range x.Names {
							if tainted[id.Name] || i >= len(x.Values) {
								continue
							}
							if mentions(x.Values[i]) {
								tainted[id.Name] = true
								changed = true
							}
						}
					}
					return true
				})
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					pkg, _ := sel.X.(*ast.Ident)
					isPkg := pkg != nil && (pkg.Name == "strings" || pkg.Name == "bytes" || pkg.Name == "slices")
					isRegexp := strings.HasPrefix(sel.Sel.Name, "Find") || sel.Sel.Name == "MatchString" || strings.HasPrefix(sel.Sel.Name, "ReplaceAll")
					if isPkg || isRegexp {
						for _, a := range x.Args {
							if mentions(a) {
								report(x, "a "+sel.Sel.Name+" call on a Message")
								break
							}
						}
					}
				case *ast.IndexExpr:
					if mentions(x.Index) {
						report(x, "a lookup by Message")
					}
				case *ast.BinaryExpr:
					if (x.Op == token.EQL || x.Op == token.NEQ) && (mentions(x.X) || mentions(x.Y)) {
						report(x, "a comparison of a Message")
					}
				case *ast.SwitchStmt:
					if x.Tag != nil && mentions(x.Tag) {
						report(x, "a switch on a Message")
					}
				}
				return true
			})
		}
	}
	return out
}

// The guard itself: each way of matching on a Message, on the field or on a local assigned from it, is
// caught, and the uses that are not matching are not.
func TestMessageGuardCatchesEveryForm(t *testing.T) {
	const prefix = "package p\nimport (\n\t\"regexp\"\n\t\"strings\"\n)\nvar re = regexp.MustCompile(`x`)\ntype F struct{ Message string }\n"
	bad := map[string]string{
		"strings on the field":          "func f(p F) bool { return strings.Contains(p.Message, \"loop\") }",
		"strings on a local":            "func f(p F) bool { m := p.Message; return strings.Contains(m, \"loop\") }",
		"strings on a local of a local": "func f(p F) bool { m := p.Message; n := m; return strings.HasPrefix(n, \"x\") }",
		"var declared from the field":   "func f(p F) bool { var m = p.Message; return strings.Contains(m, \"x\") }",
		"switch on the field":           "func f(p F) int { switch p.Message { case \"a\": return 1 }; return 0 }",
		"switch on a local":             "func f(p F) int { m := p.Message; switch m { case \"a\": return 1 }; return 0 }",
		"regexp on a local":             "func f(p F) bool { m := p.Message; return re.MatchString(m) }",
		"map lookup":                    "func f(p F, k map[string]int) int { m := p.Message; return k[m] }",
		"comparison":                    "func f(p, q F) bool { m := p.Message; return m == q.Message }",
		"concatenated local":            "func f(p F) bool { m := \"a\" + p.Message; return strings.Contains(m, \"x\") }",
	}
	for name, body := range bad {
		if got := messageMatches(t, "", map[string]string{"x.go": prefix + body}); len(got) == 0 {
			t.Errorf("%s: not caught", name)
		}
	}
	ok := map[string]string{
		"copying":                            "func f(p F) F { m := p.Message; return F{Message: m} }",
		"concatenate":                        "func f(p F) string { return \"x: \" + p.Message }",
		"a local that is not from a message": "func f(p F) bool { m := \"loop\"; return strings.Contains(m, \"l\") }",
	}
	for name, body := range ok {
		if got := messageMatches(t, "", map[string]string{"x.go": prefix + body}); len(got) != 0 {
			t.Errorf("%s: wrongly caught: %v", name, got)
		}
	}
}
