package plan

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// A finding gets its kind where its problem is made, never by reading the problem's text back: matching
// on a message is the coupling that broke the editor's comparison when a message shortened a name or
// changed a position. This walks plan's source and fails on any code that matches, searches or compares
// a Message (strings functions, regexps, map lookups and == or != on a field named Message), so a lookup
// by text cannot be added without this test being changed on purpose.
func TestNoCodeMatchesOnAMessage(t *testing.T) {
	fset := token.NewFileSet()
	pk := map[string]*ast.Package{}
	for _, dir := range []string{".", "../flowedit"} { // the editor reads findings and must not read messages either
		pkgs, err := parser.ParseDir(fset, dir, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for n, p := range pkgs {
			pk[dir+"/"+n] = p
		}
	}
	mentionsMessage := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(x ast.Node) bool {
			if sel, ok := x.(*ast.SelectorExpr); ok && sel.Sel.Name == "Message" {
				found = true
			}
			return !found
		})
		return found
	}
	for _, pkg := range pk {
		for name, file := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "strings" || id.Name == "bytes" || id.Name == "slices") {
						for _, a := range x.Args {
							if mentionsMessage(a) {
								t.Errorf("%s: %s.%s on a Message", fset.Position(x.Pos()), id.Name, sel.Sel.Name)
							}
						}
					}
					if strings.HasPrefix(sel.Sel.Name, "FindString") || sel.Sel.Name == "MatchString" || sel.Sel.Name == "ReplaceAllString" {
						for _, a := range x.Args {
							if mentionsMessage(a) {
								t.Errorf("%s: a regexp on a Message", fset.Position(x.Pos()))
							}
						}
					}
				case *ast.IndexExpr:
					if mentionsMessage(x.Index) {
						t.Errorf("%s: a lookup by Message", fset.Position(x.Pos()))
					}
				case *ast.BinaryExpr:
					if (x.Op == token.EQL || x.Op == token.NEQ) && (mentionsMessage(x.X) || mentionsMessage(x.Y)) {
						t.Errorf("%s: a comparison of a Message", fset.Position(x.Pos()))
					}
				}
				return true
			})
		}
	}
}
