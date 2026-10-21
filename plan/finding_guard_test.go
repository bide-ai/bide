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
//
// The limit: the guard works on names, one function at a time. A function that returns a concatenation
// made with a Message ("" + p.Message), where its caller then matches on the result, is not seen: a
// concatenation may be returned as text made for display. No such code exists today.
func TestNoCodeMatchesOnAMessage(t *testing.T) {
	for _, dir := range []string{".", "../flowedit", "../internal/runview"} {
		for _, msg := range messageMatches(t, dir, nil) {
			t.Error(msg)
		}
	}
}

// allowedMessageUse lists the functions that read a Message only to display it, never to decide a kind or
// a place. Each key is the file's path from the module root and the function's name; the value is the
// reason. The guard skips exactly those functions, so matching code anywhere else in the same file is
// still caught.
var allowedMessageUse = map[string]string{
	"internal/runview/style.go:assertionLines":      "colors a violated assertion's verdict and counterexample path on a terminal; the problem's place is already set by plan, and the text is only split to paint it",
	"internal/runview/style.go:problemText":         "the same colouring, for the one-line form",
	"internal/runview/diff.go:assertionRegressions": "reads an AssertionResult's own verdict text (not a ConfigProblem) to word a weakened assertion's summary",
	"flowedit/ops_structure.go:newLoopShapeError":   "decides by kind and full names (recordKey); returns the message of the one new problem only to show it",
	"internal/runview/diff.go:messageOf":            "words an AssertionResult's verdict message as a suffix, or nothing when it is empty",
}

// messageSinks are the callees a Message, or text made from one, may be passed to: formatting and
// printing, error construction, appending and writing. Anything else that receives one (a helper, a
// closure, an alias of a strings function, a method) is a failure unless the function making the call is
// in allowedMessageUse. A key is the package (or "(method)" for a call on a value) and the name.
var messageSinks = map[string]bool{
	"fmt.Sprintf": true, "fmt.Sprint": true, "fmt.Sprintln": true, "fmt.Errorf": true, "fmt.Appendf": true,
	"fmt.Fprintf": true, "fmt.Fprint": true, "fmt.Fprintln": true, "fmt.Printf": true, "fmt.Print": true, "fmt.Println": true,
	"errors.New": true, "io.WriteString": true,
	"(method).WriteString": true, "(method).Write": true, "(method).WriteByte": true,
	"(builtin).append": true,
	// Reviewed, with the reason: neither decides anything from the text.
	"plan.boundMessage": true, // shortens a long quoted name inside a message so the report stays small
	"runview.colorRisk": true, // paints risk words in a message on a terminal
}

// messageFormatters are the sinks whose result is text made from their arguments, so it holds the Message too.
var messageFormatters = map[string]bool{"fmt.Sprintf": true, "fmt.Sprint": true, "fmt.Sprintln": true, "fmt.Errorf": true, "fmt.Appendf": true}

// messageMatches returns the places in the non-test Go files of dir (or in src, a map of file name to
// source, when given) where a Message, or a value made from one, is matched, compared, switched on,
// used as a map key, returned, or passed to anything that is not a sink.
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
			if strings.HasSuffix(name, "_test.go") {
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
		path := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(fset.Position(file.Pos()).Filename)), "../")
		imports := map[string]bool{}
		for _, im := range file.Imports {
			name := strings.Trim(im.Path.Value, `"`)
			name = name[strings.LastIndex(name, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			imports[name] = true
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || allowedMessageUse[path+":"+fd.Name.Name] != "" {
				continue
			}
			// The names and field paths (q, q.m) that hold a Message: assigned from one, or from another.
			tainted := map[string]bool{}
			var key func(ast.Expr) string
			key = func(e ast.Expr) string {
				switch y := e.(type) {
				case *ast.Ident:
					return y.Name
				case *ast.SelectorExpr:
					if k := key(y.X); k != "" {
						return k + "." + y.Sel.Name
					}
				case *ast.ParenExpr:
					return key(y.X)
				}
				return ""
			}
			// callee names the function a call reaches: "pkg.Name", "(method).Name", "(builtin).Name",
			// "(conversion)" or "(closure)" (anything else, such as a local variable holding a function).
			callee := func(c *ast.CallExpr) string {
				switch f := c.Fun.(type) {
				case *ast.ArrayType, *ast.InterfaceType:
					return "(conversion)"
				case *ast.Ident:
					switch f.Name {
					case "string", "any":
						return "(conversion)"
					case "append", "len", "cap", "copy", "make", "new", "panic", "min", "max":
						return "(builtin)." + f.Name
					}
					return file.Name.Name + "." + f.Name
				case *ast.SelectorExpr:
					if id, ok := f.X.(*ast.Ident); ok && imports[id.Name] {
						return id.Name + "." + f.Sel.Name
					}
					return "(method)." + f.Sel.Name
				}
				return "(closure)"
			}
			var holds func(ast.Expr) bool
			holds = func(e ast.Expr) bool {
				switch y := e.(type) {
				case *ast.ParenExpr:
					return holds(y.X)
				case *ast.StarExpr:
					return holds(y.X)
				case *ast.UnaryExpr:
					return holds(y.X)
				case *ast.IndexExpr:
					return holds(y.X)
				case *ast.SliceExpr:
					return holds(y.X)
				case *ast.Ident:
					return tainted[y.Name]
				case *ast.SelectorExpr:
					if y.Sel.Name == "Message" {
						return true
					}
					k := key(y)
					return (k != "" && tainted[k]) || holds(y.X)
				case *ast.BinaryExpr:
					return y.Op == token.ADD && (holds(y.X) || holds(y.Y))
				case *ast.CompositeLit:
					for _, el := range y.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Message" {
								continue // building a Message from another is a copy, not a use
							}
							el = kv.Value
						}
						if holds(el) {
							return true
						}
					}
				case *ast.CallExpr:
					c := callee(y)
					if c == "(conversion)" || c == "(builtin).append" || messageFormatters[c] {
						for _, a := range y.Args {
							if holds(a) {
								return true
							}
						}
					}
				}
				return false
			}
			// bare is a Message itself (or a conversion or copy of one), not text made with it.
			var bare func(ast.Expr) bool
			bare = func(e ast.Expr) bool {
				switch y := e.(type) {
				case *ast.ParenExpr:
					return bare(y.X)
				case *ast.CallExpr:
					return callee(y) == "(conversion)" && len(y.Args) == 1 && bare(y.Args[0])
				case *ast.BinaryExpr:
					return false
				}
				return holds(e)
			}
			// mentions is holds, looking anywhere inside the expression.
			mentions := func(n ast.Node) bool {
				found := false
				ast.Inspect(n, func(x ast.Node) bool {
					if e, ok := x.(ast.Expr); ok && holds(e) {
						found = true
					}
					return !found
				})
				return found
			}
			for changed := true; changed; {
				changed = false
				taint := func(l ast.Expr) {
					k := key(l)
					if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "Message" {
						return // assigned to another Message
					}
					if k != "" && !tainted[k] {
						tainted[k] = true
						changed = true
					}
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.AssignStmt:
						for i, l := range x.Lhs {
							rhs := x.Rhs[0]
							if len(x.Rhs) == len(x.Lhs) {
								rhs = x.Rhs[i]
							}
							if holds(rhs) {
								taint(l)
							}
						}
					case *ast.ValueSpec:
						for i, id := range x.Names {
							if i < len(x.Values) && holds(x.Values[i]) {
								taint(id)
							}
						}
					case *ast.RangeStmt:
						if holds(x.X) {
							if x.Key != nil {
								taint(x.Key)
							}
							if x.Value != nil {
								taint(x.Value)
							}
						}
					}
					return true
				})
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					c := callee(x)
					if c == "(conversion)" || messageSinks[c] {
						return true
					}
					for _, a := range x.Args {
						if holds(a) {
							report(x, "a Message passed to "+c+", which is not a sink")
							break
						}
					}
				case *ast.ReturnStmt:
					for _, r := range x.Results {
						if bare(r) {
							report(x, "a Message returned bare, where a caller can match on it")
							break
						}
					}
				case *ast.SendStmt:
					if holds(x.Value) {
						report(x, "a Message sent on a channel, where a receiver can match on it")
					}
				case *ast.IndexExpr:
					if mentions(x.Index) {
						report(x, "a lookup by Message")
					}
				case *ast.BinaryExpr:
					if id, ok := x.Y.(*ast.Ident); ok && id.Name == "nil" {
						return true // a pointer or an interface is not a text
					}
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
	const prefix = "package p\nimport (\n\t\"errors\"\n\t\"fmt\"\n\t\"regexp\"\n\t\"strings\"\n)\nvar re = regexp.MustCompile(`x`)\ntype F struct{ Message string }\n"
	bad := map[string]string{
		"strings on the field":           "func f(p F) bool { return strings.Contains(p.Message, \"loop\") }",
		"strings on a local":             "func f(p F) bool { m := p.Message; return strings.Contains(m, \"loop\") }",
		"strings on a local of a local":  "func f(p F) bool { m := p.Message; n := m; return strings.HasPrefix(n, \"x\") }",
		"var declared from the field":    "func f(p F) bool { var m = p.Message; return strings.Contains(m, \"x\") }",
		"switch on the field":            "func f(p F) int { switch p.Message { case \"a\": return 1 }; return 0 }",
		"switch on a local":              "func f(p F) int { m := p.Message; switch m { case \"a\": return 1 }; return 0 }",
		"regexp Find on a local":         "func f(p F) string { m := p.Message; return re.FindString(m) }",
		"regexp ReplaceAll on a local":   "func f(p F) string { m := p.Message; return re.ReplaceAllString(m, \"\") }",
		"inequality":                     "func f(p F) bool { m := p.Message; return m != \"\" }",
		"regexp on a local":              "func f(p F) bool { m := p.Message; return re.MatchString(m) }",
		"map lookup":                     "func f(p F, k map[string]int) int { m := p.Message; return k[m] }",
		"comparison":                     "func f(p, q F) bool { m := p.Message; return m == q.Message }",
		"a helper":                       "func has(s string) bool { return strings.Contains(s, \"x\") }\nfunc f(p F) bool { return has(p.Message) }",
		"a closure":                      "func f(p F) bool { g := func(s string) bool { return strings.Contains(s, \"x\") }; return g(p.Message) }",
		"an alias of a strings function": "func f(p F) bool { has := strings.Contains; return has(p.Message, \"x\") }",
		"a struct literal copy":          "func f(p F) bool { q := struct{ m string }{p.Message}; return strings.Contains(q.m, \"x\") }",
		"a keyed struct literal copy":    "func f(p F) bool { q := struct{ m string }{m: p.Message}; return strings.Contains(q.m, \"x\") }",
		"a field copy":                   "func f(p F) bool { var q struct{ m string }; q.m = p.Message; return strings.Contains(q.m, \"x\") }",
		"a range copy":                   "func f(ps []F) bool { for _, p := range ps { m := p.Message; _ = m }; return false }\nfunc g(p F) bool { for _, c := range []byte(p.Message) { return c == 'x' }; return false }",
		"returned bare":                  "func f(p F) string { return p.Message }",
		"returned bare from a local":     "func f(p F) string { m := p.Message; return m }",
		"a method":                       "func f(p F, r *regexp.Regexp) bool { return r.MatchString(p.Message) }",
		"formatted, then matched":        "func f(p F) bool { s := fmt.Sprintf(\"%s\", p.Message); return strings.Contains(s, \"x\") }",
		"appended, then indexed":         "func f(p F) bool { var xs []string; xs = append(xs, p.Message); return strings.Contains(xs[0], \"x\") }",
		"appended, then ranged":          "func f(p F) bool { xs := append([]string{}, p.Message); for _, x := range xs { if x == \"a\" { return true } }; return false }",
		"a channel send":                 "func f(p F, ch chan string) { ch <- p.Message }",
		"a channel send of a local":      "func f(p F, ch chan string) { m := p.Message; ch <- m }",
		"concatenated local":             "func f(p F) bool { m := \"a\" + p.Message; return strings.Contains(m, \"x\") }",
	}
	for name, body := range bad {
		if got := messageMatches(t, "", map[string]string{"x.go": prefix + body}); len(got) == 0 {
			t.Errorf("%s: not caught", name)
		}
	}
	ok := map[string]string{
		"copying":                            "func f(p F) F { m := p.Message; return F{Message: m} }",
		"a nil check on a pointer Message":   "func f(g *struct{ Message *int }) bool { return g.Message != nil }",
		"a struct holding one":               "func f(p F) bool { q := struct{ m F }{m: p}; return q.m == p }",
		"into Sprintf":                       "func f(p F) string { return fmt.Sprintf(\"x: %s\", p.Message) }",
		"into Errorf":                        "func f(p F) error { return fmt.Errorf(\"x: %w\", errors.New(p.Message)) }",
		"into a builder":                     "func f(p F) string { var b strings.Builder; b.WriteString(p.Message); return b.String() }",
		"appended":                           "func f(p F, out []string) []string { return append(out, p.Message) }",
		"assigned to a Message":              "func f(p F, q *F) { q.Message = p.Message }",
		"appended and not read":              "func f(p F) { var xs []string; xs = append(xs, p.Message); _ = xs }",
		"a channel send of other text":       "func f(ch chan string) { ch <- \"x\" }",
		"concatenate":                        "func f(p F) string { return \"x: \" + p.Message }",
		"a local that is not from a message": "func f(p F) bool { m := \"loop\"; return strings.Contains(m, \"l\") }",
	}
	for name, body := range ok {
		if got := messageMatches(t, "", map[string]string{"x.go": prefix + body}); len(got) != 0 {
			t.Errorf("%s: wrongly caught: %v", name, got)
		}
	}
}

// Every reviewed exception is still needed: with one removed, the guard fails on the code it excused. An
// entry that no longer excuses anything is deleted, so the lists only shrink.
func TestMessageGuardExceptionsAreStillNeeded(t *testing.T) {
	all := func() int {
		n := 0
		for _, dir := range []string{".", "../flowedit", "../internal/runview"} {
			n += len(messageMatches(t, dir, nil))
		}
		return n
	}
	if n := all(); n != 0 {
		t.Fatalf("the guard fails before any exception is removed (%d)", n)
	}
	for k, reason := range allowedMessageUse {
		delete(allowedMessageUse, k)
		if all() == 0 {
			t.Errorf("allowedMessageUse[%q] excuses nothing: delete it", k)
		}
		allowedMessageUse[k] = reason
	}
	for _, k := range []string{"plan.boundMessage", "runview.colorRisk"} {
		delete(messageSinks, k)
		if all() == 0 {
			t.Errorf("messageSinks[%q] excuses nothing: delete it", k)
		}
		messageSinks[k] = true
	}
}
