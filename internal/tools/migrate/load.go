package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Package is one package of the module being migrated, parsed and type-checked from source. A
// package with tests is loaded once, as its test variant (its own files and its in-package test
// files); its external test package is a Package of its own.
type Package struct {
	ID    string // the go command's ID: the import path, with " [p.test]" for a test variant
	Path  string // the import path
	Name  string
	Dir   string
	Fset  *token.FileSet
	Files []*File
	Types *types.Package
	Info  *types.Info
	// TypeErrors are the type checker's errors. Old code type-checks cleanly; code that is
	// already partly migrated (a rebase brought in old call sites) does not, and the rules then
	// work from the partial information the checker recorded.
	TypeErrors []error
}

// File is one source file of a Package.
type File struct {
	Path string // absolute
	Src  []byte
	AST  *ast.File
	Pkg  *Package
}

type listed struct {
	ImportPath string
	Name       string
	Dir        string
	ForTest    string
	DepOnly    bool
	Export     string
	GoFiles    []string
	CgoFiles   []string
	ImportMap  map[string]string
	Error      *struct{ Err string }
}

// Load lists the packages patterns match in the module at dir (with their test variants), and
// parses and type-checks each from source. Imports are read from the export data the go
// command builds, so a package is checked against exactly what it compiles against.
//
// overlay, when not empty, holds file contents (by absolute path) that stand for the files on
// disk: the go command builds with them (-overlay), and the packages are parsed from them. flags
// are further go command flags (CheckModule's -modfile).
func Load(dir string, patterns []string, overlay map[string][]byte, flags ...string) ([]*Package, error) {
	args := append([]string{"list", "-e", "-test", "-deps", "-export",
		"-json=ImportPath,Name,Dir,ForTest,DepOnly,Export,GoFiles,CgoFiles,ImportMap,Error"}, flags...)
	if len(overlay) > 0 {
		tmp, err := os.MkdirTemp("", "migrate-overlay")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		replace := map[string]string{}
		i := 0
		for path, src := range overlay {
			f := filepath.Join(tmp, fmt.Sprintf("%d.go", i))
			i++
			if err := os.WriteFile(f, src, 0o644); err != nil {
				return nil, err
			}
			replace[path] = f
		}
		b, err := json.Marshal(map[string]any{"Replace": replace})
		if err != nil {
			return nil, err
		}
		of := filepath.Join(tmp, "overlay.json")
		if err := os.WriteFile(of, b, 0o644); err != nil {
			return nil, err
		}
		args = append(args, "-overlay="+of)
	}
	args = append(args, patterns...)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var all []*listed
	byID := map[string]*listed{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listed
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list output: %v", err)
		}
		all = append(all, &p)
		byID[p.ImportPath] = &p
	}
	hasTestVariant := map[string]bool{}
	for _, p := range all {
		if p.ForTest != "" && !strings.HasSuffix(basePath(p.ImportPath), "_test") {
			hasTestVariant[p.ForTest] = true
		}
	}
	var pkgs []*Package
	for _, p := range all {
		switch {
		case p.DepOnly:
			continue
		case strings.HasSuffix(p.ImportPath, ".test") && !strings.Contains(p.ImportPath, " "):
			continue // the generated test main
		case p.ForTest == "" && hasTestVariant[p.ImportPath]:
			continue // its test variant holds the same files and more
		case len(p.GoFiles)+len(p.CgoFiles) == 0:
			continue
		}
		pkg, err := check(p, byID, overlay)
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, pkg)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].ID < pkgs[j].ID })
	return pkgs, nil
}

// basePath strips a test variant's " [p.test]" from an ID.
func basePath(id string) string {
	path, _, _ := strings.Cut(id, " ")
	return path
}

// check parses p's files and type-checks them, importing through p's ImportMap.
func check(p *listed, byID map[string]*listed, overlay map[string][]byte) (*Package, error) {
	fset := token.NewFileSet()
	pkg := &Package{ID: p.ImportPath, Path: basePath(p.ImportPath), Name: p.Name, Dir: p.Dir, Fset: fset}
	var syntax []*ast.File
	for _, name := range append(append([]string{}, p.GoFiles...), p.CgoFiles...) {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(p.Dir, name)
		}
		src, ok := overlay[path]
		if !ok {
			var err error
			if src, err = os.ReadFile(path); err != nil {
				return nil, err
			}
		}
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %v", path, err)
		}
		pkg.Files = append(pkg.Files, &File{Path: path, Src: src, AST: f, Pkg: pkg})
		syntax = append(syntax, f)
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		id := path
		if m, ok := p.ImportMap[path]; ok {
			id = m
		}
		l := byID[id]
		if l == nil || l.Export == "" {
			why := "no export data"
			if l != nil && l.Error != nil {
				why = l.Error.Err
			}
			return nil, fmt.Errorf("import %s: %s", path, why)
		}
		return os.Open(l.Export)
	})
	pkg.Info = &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Implicits:  map[ast.Node]types.Object{},
		Scopes:     map[ast.Node]*types.Scope{},
		Instances:  map[*ast.Ident]types.Instance{},
	}
	conf := types.Config{
		Importer: imp,
		Error:    func(err error) { pkg.TypeErrors = append(pkg.TypeErrors, err) },
	}
	tp, err := conf.Check(pkg.Path, fset, syntax, pkg.Info)
	if tp == nil {
		return nil, fmt.Errorf("type-check %s: %v", p.ImportPath, err)
	}
	pkg.Types = tp
	return pkg, nil
}
