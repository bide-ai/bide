package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A Finding is one documentation gap: an exported identifier without a doc comment, a doc
// comment that does not start with the identifier's name, or a package without a package comment.
type Finding struct {
	// Pos is the position of the identifier (or of the package clause), with the file name
	// relative to the checked root.
	Pos token.Position
	// Key names the identifier as the allowlist does: the package directory relative to the root
	// (slash-separated, "." for the root), a space, then Name, Recv.Name for a method, or the
	// word "package" for the package comment.
	Key string
	// Msg says what is wrong.
	Msg string
}

// String formats f as file:line:col: key: message.
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s: %s", f.Pos.Filename, f.Pos.Line, f.Pos.Column, f.Key, f.Msg)
}

// skipDir reports whether the walk skips a directory: the go tool ignores testdata and names
// starting with "." or "_", vendor holds other people's code, and node_modules belongs to the
// docs site.
func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// Check parses every non-test Go file under root, across all modules below it, and returns the
// findings sorted by position. Generated files are skipped. Exported identifiers in package main
// are not API, so only main's package comment is checked.
func Check(root string) ([]Finding, error) {
	fset := token.NewFileSet()
	type pkgKey struct{ dir, name string }
	pkgs := map[pkgKey][]*ast.File{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, filepath.ToSlash(rel), src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if ast.IsGenerated(f) {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		k := pkgKey{dir, f.Name.Name}
		pkgs[k] = append(pkgs[k], f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []Finding
	for k, files := range pkgs {
		slices.SortFunc(files, func(a, b *ast.File) int {
			return strings.Compare(fset.File(a.Package).Name(), fset.File(b.Package).Name())
		})
		c := &checker{fset: fset, dir: k.dir}
		c.pkg(k.name, files)
		out = append(out, c.out...)
	}
	slices.SortFunc(out, func(a, b Finding) int {
		if c := strings.Compare(a.Pos.Filename, b.Pos.Filename); c != 0 {
			return c
		}
		if a.Pos.Line != b.Pos.Line {
			return a.Pos.Line - b.Pos.Line
		}
		if a.Pos.Column != b.Pos.Column {
			return a.Pos.Column - b.Pos.Column
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out, nil
}

type checker struct {
	fset *token.FileSet
	dir  string
	out  []Finding
}

func (c *checker) report(pos token.Pos, name, format string, args ...any) {
	c.out = append(c.out, Finding{Pos: c.fset.Position(pos), Key: c.dir + " " + name, Msg: fmt.Sprintf(format, args...)})
}

func (c *checker) pkg(name string, files []*ast.File) {
	// Both package findings sit on a package clause: of the first file (by name) when no file
	// has a package comment, else of the first file that has one.
	var docFile *ast.File
	for _, f := range files {
		if f.Doc != nil && strings.TrimSpace(f.Doc.Text()) != "" {
			docFile = f
			break
		}
	}
	switch {
	case docFile == nil:
		c.report(files[0].Package, "package", "package %s has no package comment", name)
	case name != "main" && !startsWithName(docFile.Doc.Text(), "Package "+name):
		c.report(docFile.Package, "package", "package comment should start with %q", "Package "+name)
	}
	if name == "main" {
		return
	}
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				c.funcDecl(d)
			case *ast.GenDecl:
				c.genDecl(d)
			}
		}
	}
}

func (c *checker) funcDecl(d *ast.FuncDecl) {
	if !d.Name.IsExported() {
		return
	}
	kind, key := "func", d.Name.Name
	if d.Recv != nil {
		if len(d.Recv.List) == 0 {
			return
		}
		recv := recvName(d.Recv.List[0].Type)
		if !token.IsExported(recv) {
			return
		}
		kind, key = "method", recv+"."+d.Name.Name
	}
	c.checkDoc(d.Name.Pos(), key, kind, d.Doc, []string{d.Name.Name})
}

// recvName returns the base type name of a method receiver: T for T, *T, T[P], *T[P, Q].
func recvName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

func (c *checker) genDecl(d *ast.GenDecl) {
	// A declaration with one spec and no parentheses reads its doc from the declaration. In a
	// parenthesized group, a type needs its own doc; a const or var may rely on the group's doc,
	// which describes the group rather than one name, so its first word is not checked.
	single := !d.Lparen.IsValid()
	switch d.Tok {
	case token.TYPE:
		for _, s := range d.Specs {
			ts := s.(*ast.TypeSpec)
			if !ts.Name.IsExported() {
				continue
			}
			doc := ts.Doc
			if doc == nil && single {
				doc = d.Doc
			}
			c.checkDoc(ts.Name.Pos(), ts.Name.Name, "type", doc, []string{ts.Name.Name})
		}
	case token.CONST, token.VAR:
		kind := "const"
		if d.Tok == token.VAR {
			kind = "var"
		}
		for _, s := range d.Specs {
			vs := s.(*ast.ValueSpec)
			var names []string
			for _, n := range vs.Names {
				if n.IsExported() {
					names = append(names, n.Name)
				}
			}
			if len(names) == 0 {
				continue
			}
			doc := vs.Doc
			if doc == nil && single {
				doc = d.Doc
			}
			if doc == nil && d.Doc != nil && strings.TrimSpace(d.Doc.Text()) != "" {
				continue // documented by its group
			}
			for _, n := range vs.Names {
				if n.IsExported() {
					c.checkDoc(n.Pos(), n.Name, kind, doc, names)
				}
			}
		}
	}
}

// checkDoc reports a missing doc comment, or one that does not start with one of names (a
// spec such as "var A, B = ..." may be documented by either name). A type's comment may put
// "A", "An" or "The" before the name.
func (c *checker) checkDoc(pos token.Pos, key, kind string, doc *ast.CommentGroup, names []string) {
	text := ""
	if doc != nil {
		text = doc.Text()
	}
	if strings.TrimSpace(text) == "" {
		c.report(pos, key, "exported %s %s has no doc comment", kind, key)
		return
	}
	for _, n := range names {
		if startsWithName(text, n) {
			return
		}
		// Go's doc comment convention lets a type's comment open with an article: "A Reader ...".
		if kind == "type" {
			for _, article := range []string{"A ", "An ", "The "} {
				if startsWithName(text, article+n) {
					return
				}
			}
		}
	}
	c.report(pos, key, "doc comment of exported %s %s should start with %q", kind, key, names[0])
}

// startsWithName reports whether text, after leading space, starts with name followed by the
// end of the text or by a rune that cannot continue an identifier, so "Foo" does not accept
// "Foobar does ...".
func startsWithName(text, name string) bool {
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
	rest, ok := strings.CutPrefix(text, name)
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
}

// An Allowlist is the set of findings tolerated today, one key per line. It may only shrink: an
// entry that no longer matches a finding is itself an error.
type Allowlist struct {
	keys []string
}

// ParseAllowlist reads an allowlist: one "dir name" key per line, blank lines and lines starting
// with "#" ignored. Entries must be unique and sorted, so the file has one canonical form and a
// diff shows exactly which entries a change adds or removes.
func ParseAllowlist(data []byte) (*Allowlist, error) {
	a := &Allowlist{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	line := 0
	for sc.Scan() {
		line++
		s := sc.Text()
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if s != strings.TrimSpace(s) || len(strings.Fields(s)) != 2 || strings.Count(s, " ") != 1 {
			return nil, fmt.Errorf("line %d: malformed entry %q: want \"dir name\"", line, s)
		}
		if n := len(a.keys); n > 0 {
			switch prev := a.keys[n-1]; {
			case s == prev:
				return nil, fmt.Errorf("line %d: duplicate entry %q", line, s)
			case s < prev:
				return nil, fmt.Errorf("line %d: entry %q is out of order (after %q); keep the file sorted", line, s, prev)
			}
		}
		a.keys = append(a.keys, s)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return a, nil
}

// Apply returns the findings the allowlist does not cover and the entries that match no finding
// (the identifier is now documented, or no longer exists), in order.
func (a *Allowlist) Apply(findings []Finding) (unlisted []Finding, stale []string) {
	used := map[string]bool{}
	for _, f := range findings {
		if _, ok := slices.BinarySearch(a.keys, f.Key); ok {
			used[f.Key] = true
			continue
		}
		unlisted = append(unlisted, f)
	}
	for _, k := range a.keys {
		if !used[k] {
			stale = append(stale, k)
		}
	}
	return unlisted, stale
}
