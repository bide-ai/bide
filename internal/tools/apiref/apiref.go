// Package apiref generates the SDK API reference, docs/reference/api/, from the Go sources of
// every published module: for each non-internal package its import path, its synopsis and its
// stability label, then every exported constant, variable, function and type (with the
// constructors and methods go/doc groups under it) as the declaration and the first sentence of
// its doc comment. It uses only the standard library (go/parser, go/doc, go/printer), so it adds
// no dependency, and the output is a pure function of the sources: sorted, with no timestamps.
//
// The pages are checked by TestAPIReference, which fails when they differ from the sources;
// regenerate with go test ./internal/tools/apiref -run TestAPIReference -update.
//
// Which packages are listed: every package of every module in the repository except
// the modules that are never published (the REPO_ONLY and COMMANDS lists of scripts/release.sh),
// directories named internal, testdata, examples, assets or cmd, and package main. Files are
// selected for linux/amd64 with cgo enabled, so a symbol that exists only on another platform is
// not listed.
package apiref

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A Module is one published Go module and its packages.
type Module struct {
	Path     string // module path
	Dir      string // slash-separated directory relative to the repository root; "." for the core
	Packages []*Package
}

// A Package is one documented package.
type Package struct {
	ImportPath string
	Rel        string // ImportPath relative to the root module's path: the key of the label tables
	Name       string
	Synopsis   string
	Label      string // the stability label of the package
	Consts     []Value
	Vars       []Value
	Funcs      []Entry
	Types      []Type
	hasDoc     bool
}

// An Entry is one function, method or type: its declaration and the first sentence of its doc.
type Entry struct {
	Name     string // "Name", or "Type.Method" for a method
	Decl     string
	Sentence string
	File     string // base name of the file that declares it
}

// A Value is one const or var group.
type Value struct {
	Items []Item
}

// An Item is one exported name of a Value. Its sentence is its own doc comment, or the group's.
type Item struct {
	Name     string
	Spec     string // "const Name Type = value", the value elided when long or implicit
	Sentence string
	File     string
}

// A Type is a type with the constants, variables, constructors and methods go/doc groups under it.
type Type struct {
	Entry
	Consts  []Value
	Vars    []Value
	Funcs   []Entry
	Methods []Entry
}

// Slug is the file name of the module's page under docs/reference/api/, without the extension.
func (m *Module) Slug() string {
	if m.Dir == "." {
		return "core"
	}
	return strings.ReplaceAll(m.Dir, "/", "-")
}

// Modules finds the published modules below root and parses their packages.
func Modules(root string) ([]*Module, error) {
	skip, err := unpublished(root)
	if err != nil {
		return nil, err
	}
	var dirs []string // module directories, relative
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skippedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			rel, err := filepath.Rel(root, filepath.Dir(p))
			if err != nil {
				return err
			}
			dirs = append(dirs, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	isModule := map[string]bool{}
	for _, d := range dirs {
		isModule[d] = true
	}
	var mods []*Module
	for _, dir := range dirs {
		if skip[dir] || hasElem(dir, "cmd") {
			continue
		}
		modPath, err := modulePath(filepath.Join(root, filepath.FromSlash(dir), "go.mod"))
		if err != nil {
			return nil, err
		}
		m := &Module{Path: modPath, Dir: dir}
		err = filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel != dir && (skippedDir(d.Name()) || isModule[rel]) {
				return filepath.SkipDir
			}
			sub := strings.TrimPrefix(strings.TrimPrefix(rel, dir), "/")
			imp := modPath
			if sub != "" {
				imp = modPath + "/" + sub
			}
			pkg, err := parsePackage(p, imp)
			if err != nil {
				return err
			}
			if pkg != nil {
				m.Packages = append(m.Packages, pkg)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(m.Packages) == 0 {
			continue
		}
		sort.Slice(m.Packages, func(i, j int) bool { return m.Packages[i].ImportPath < m.Packages[j].ImportPath })
		mods = append(mods, m)
	}
	if len(mods) == 0 || mods[0].Dir != "." {
		return nil, fmt.Errorf("%s: no root module with documented packages", root)
	}
	for _, m := range mods {
		for _, p := range m.Packages {
			p.Rel = strings.TrimPrefix(p.ImportPath, mods[0].Path+"/")
		}
	}
	return mods, nil
}

// skippedDir reports whether a directory is never documented (and not descended into).
func skippedDir(name string) bool {
	switch name {
	case "internal", "testdata", "examples", "assets", "cmd", "node_modules", "vendor", "docs-site":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func hasElem(dir, elem string) bool {
	for _, e := range strings.Split(dir, "/") {
		if e == elem {
			return true
		}
	}
	return false
}

// releaseVar matches NAME="..." in scripts/release.sh.
var releaseVar = regexp.MustCompile(`(?m)^(COMMANDS|REPO_ONLY)="([^"]*)"`)

// unpublished returns the directories of the modules the release never tags: the program
// modules and the repo-only modules, read from scripts/release.sh so the lists cannot diverge.
func unpublished(root string) (map[string]bool, error) {
	b, err := os.ReadFile(filepath.Join(root, "scripts", "release.sh"))
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	found := 0
	for _, m := range releaseVar.FindAllStringSubmatch(string(b), -1) {
		found++
		for _, d := range strings.Fields(m[2]) {
			skip[d] = true
		}
	}
	if found != 2 {
		return nil, fmt.Errorf("scripts/release.sh: want COMMANDS and REPO_ONLY, found %d of them", found)
	}
	return skip, nil
}

func modulePath(gomod string) (string, error) {
	b, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`), nil
		}
	}
	return "", fmt.Errorf("%s: no module line", gomod)
}

// parsePackage parses the package in dir, or returns nil when dir holds no documented package
// (no Go files for the reference platform, or package main).
func parsePackage(dir, importPath string) (*Package, error) {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = "linux", "amd64", true
	ctx.BuildTags, ctx.ReleaseTags = nil, build.Default.ReleaseTags
	bp, err := ctx.ImportDir(dir, 0)
	if err != nil {
		if _, ok := err.(*build.NoGoError); ok {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	if bp.Name == "main" {
		return nil, nil
	}
	fset := token.NewFileSet()
	var files []*ast.File
	fileOf := map[*ast.File]string{}
	for _, name := range append(append([]string{}, bp.GoFiles...), bp.CgoFiles...) {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		fileOf[f] = name
	}
	sort.Slice(files, func(i, j int) bool { return fileOf[files[i]] < fileOf[files[j]] })
	// Where each declaration is, before go/doc filters and rearranges the files.
	where := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			recordWhere(where, fileOf[f], d)
		}
	}
	dp, err := doc.NewFromFiles(fset, files, importPath)
	if err != nil {
		return nil, err
	}
	b := &builder{dp: dp, fset: fset, where: where}
	pkg := &Package{ImportPath: importPath, Name: bp.Name, Synopsis: sentence(dp.Doc), hasDoc: strings.TrimSpace(dp.Doc) != ""}
	pkg.Consts, pkg.Vars = b.values(dp.Consts), b.values(dp.Vars)
	for _, f := range dp.Funcs {
		pkg.Funcs = append(pkg.Funcs, b.fn(f, ""))
	}
	for _, t := range dp.Types {
		pt := Type{Entry: Entry{Name: t.Name, Sentence: sentence(t.Doc), File: where[t.Name]}}
		pt.Decl = b.print(t.Decl)
		pt.Consts, pt.Vars = b.values(t.Consts), b.values(t.Vars)
		for _, f := range t.Funcs {
			pt.Funcs = append(pt.Funcs, b.fn(f, ""))
		}
		for _, f := range t.Methods {
			pt.Methods = append(pt.Methods, b.fn(f, t.Name))
		}
		pkg.Types = append(pkg.Types, pt)
	}
	return pkg, nil
}

// A builder turns go/doc output into the Package model.
type builder struct {
	dp    *doc.Package
	fset  *token.FileSet
	where map[string]string
}

func (b *builder) values(vs []*doc.Value) []Value {
	var out []Value
	for _, v := range vs {
		val := Value{}
		group := sentence(v.Doc)
		kind := v.Decl.Tok.String()
		var typ ast.Expr // the type an implicit constant repeats
		for _, s := range v.Decl.Specs {
			vs := s.(*ast.ValueSpec)
			text := sentence(vs.Doc.Text())
			if text == "" {
				text = group
			}
			implicit := kind == "const" && vs.Type == nil && len(vs.Values) == 0
			if kind == "const" && !implicit {
				typ = vs.Type
			}
			for i, n := range vs.Names {
				if !n.IsExported() {
					continue
				}
				spec := kind + " " + n.Name
				switch {
				case implicit:
					if typ != nil {
						spec += " " + b.expr(typ)
					}
				default:
					if vs.Type != nil {
						spec += " " + b.expr(vs.Type)
					}
					if len(vs.Values) == len(vs.Names) {
						if val := b.expr(vs.Values[i]); len(val) <= 40 && !strings.Contains(val, "iota") {
							spec += " = " + val
						}
					}
				}
				val.Items = append(val.Items, Item{Name: n.Name, Spec: spec, Sentence: text, File: b.where[n.Name]})
			}
		}
		out = append(out, val)
	}
	return out
}

// expr prints an expression on one line.
func (b *builder) expr(e ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, b.fset, e); err != nil {
		return "?"
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}

func (b *builder) fn(f *doc.Func, recv string) Entry {
	name := f.Name
	if recv != "" {
		name = recv + "." + f.Name
	}
	return Entry{Name: name, Decl: b.print(f.Decl), Sentence: sentence(f.Doc), File: b.where[name]}
}

// Undocumented lists the exported symbols of the packages that have no doc comment, as
// "import/path Name". doccheck enforces the same rule on every module; this keeps the page from
// ever listing a symbol with nothing to say.
func Undocumented(mods []*Module) []string {
	var out []string
	for _, m := range mods {
		for _, p := range m.Packages {
			if !p.hasDoc {
				out = append(out, p.ImportPath+" (package)")
			}
			p.each(func(kind, name, sentence, _ string) {
				if sentence == "" {
					out = append(out, p.ImportPath+" "+name)
				}
			})
		}
	}
	sort.Strings(out)
	return out
}

// each calls f for every exported symbol of the package.
func (p *Package) each(f func(kind, name, sentence, file string)) {
	vals := func(vs []Value, kind string) {
		for _, v := range vs {
			for _, it := range v.Items {
				f(kind, it.Name, it.Sentence, it.File)
			}
		}
	}
	vals(p.Consts, "const")
	vals(p.Vars, "var")
	for _, e := range p.Funcs {
		f("func", e.Name, e.Sentence, e.File)
	}
	for _, t := range p.Types {
		f("type", t.Name, t.Sentence, t.File)
		vals(t.Consts, "const")
		vals(t.Vars, "var")
		for _, e := range t.Funcs {
			f("func", e.Name, e.Sentence, e.File)
		}
		for _, e := range t.Methods {
			f("method", e.Name, e.Sentence, e.File)
		}
	}
}

// print renders a declaration in gofmt style, without comments.
func (b *builder) print(n ast.Node) string {
	ast.Inspect(n, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			n.Doc = nil
		case *ast.GenDecl:
			n.Doc = nil
		case *ast.ValueSpec:
			n.Doc, n.Comment = nil, nil
		case *ast.TypeSpec:
			n.Doc, n.Comment = nil, nil
		case *ast.Field:
			n.Doc, n.Comment = nil, nil
		}
		return true
	})
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 4}
	if err := cfg.Fprint(&buf, b.fset, n); err != nil {
		return fmt.Sprintf("// %v", err)
	}
	return buf.String()
}

// recordWhere notes the file of every exported name a declaration introduces: "Name" for a
// constant, variable, type or function, and "Type.Method" for a method.
func recordWhere(where map[string]string, file string, d ast.Decl) {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			where[d.Name.Name] = file
			return
		}
		where[recvName(d)+"."+d.Name.Name] = file
	case *ast.GenDecl:
		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				where[s.Name.Name] = file
			case *ast.ValueSpec:
				for _, n := range s.Names {
					where[n.Name] = file
				}
			}
		}
	}
}

// recvName is the receiver type name of a method, without pointer or type parameters.
func recvName(d *ast.FuncDecl) string {
	t := d.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.ParenExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

// abbreviations end in a period without ending a sentence.
var abbreviations = map[string]bool{"e.g.": true, "i.e.": true, "etc.": true, "vs.": true, "cf.": true}

// sentence is the first sentence of a doc comment's first paragraph, on one line: up to the first
// period followed by a space, except after an abbreviation such as "e.g.". A comment with no such
// period yields its whole first paragraph. It is go/doc's Synopsis with the abbreviations handled.
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "\n\n"); i >= 0 {
		text = text[:i]
	}
	// The page states no claim with an em dash: the sources that have one read as a comma here.
	text = strings.NewReplacer(" — ", ", ", "—", "-").Replace(strings.Join(strings.Fields(text), " "))
	for i := 0; i < len(text); i++ {
		if text[i] != '.' || (i+1 < len(text) && text[i+1] != ' ') {
			continue
		}
		start := strings.LastIndexByte(text[:i], ' ') + 1
		if abbreviations[strings.ToLower(strings.TrimLeft(text[start:i+1], "(\"'`"))] {
			continue
		}
		return text[:i+1]
	}
	return text
}
