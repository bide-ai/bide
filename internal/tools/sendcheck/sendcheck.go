package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A finding is a goroutine that hands a result to another goroutine (a channel send or close) and
// also has a deferred call that is not a plain signal: a deferred decrement, release or unlock runs
// after the result is sent, so the receiver can act on state the goroutine has not yet put right.
type finding struct {
	Pos    token.Position
	Key    string // file|enclosing function|deferred call
	Sends  []string
	Defers []string
}

func exprString(fset *token.FileSet, n ast.Node) string {
	var b bytes.Buffer
	printer.Fprint(&b, fset, n)
	s := strings.Join(strings.Fields(b.String()), " ")
	if len(s) > 60 {
		s = s[:60] + "..."
	}
	return s
}

// shallow visits the nodes of body that are not inside a nested function literal.
func shallow(body ast.Node, visit func(ast.Node)) {
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok && n != body {
			return false
		}
		visit(n)
		return true
	})
}

// benign defers signal or clean up without exposing state to the receiver: waiting-group Done,
// cancel functions, closing a channel (the signal itself, last), helper markers.
func benign(call string) bool {
	switch {
	case strings.HasSuffix(call, ".Done()"), strings.HasPrefix(call, "cancel"), strings.HasPrefix(call, "close("),
		strings.HasSuffix(call, "t.Helper()"), strings.HasSuffix(call, ".Stop()"):
		return true
	}
	return false
}

func analyze(fset *token.FileSet, body *ast.BlockStmt) (sends, defers []string) {
	shallow(body, func(n ast.Node) {
		switch x := n.(type) {
		case *ast.SendStmt:
			sends = append(sends, "send "+exprString(fset, x.Chan))
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "close" && len(x.Args) == 1 {
				sends = append(sends, "close "+exprString(fset, x.Args[0]))
			}
		case *ast.DeferStmt:
			if c := exprString(fset, x.Call); !benign(c) {
				defers = append(defers, c)
			}
		}
	})
	return sends, defers
}

// scan returns the findings in the Go files below root, test files included.
func scan(root string) ([]finding, error) {
	fset := token.NewFileSet()
	type file struct {
		path string
		f    *ast.File
	}
	var files []file
	decls := map[string][]*ast.FuncDecl{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "node_modules" || n == "testdata" || n == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil // not this tool's to report
		}
		rel, _ := filepath.Rel(root, p)
		files = append(files, file{filepath.ToSlash(rel), f})
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				decls[fd.Name.Name] = append(decls[fd.Name.Name], fd)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []finding
	for _, fl := range files {
		for _, d := range fl.f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				g, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				var body *ast.BlockStmt
				switch fun := g.Call.Fun.(type) {
				case *ast.FuncLit:
					body = fun.Body
				case *ast.Ident, *ast.SelectorExpr:
					name := ""
					if id, ok := fun.(*ast.Ident); ok {
						name = id.Name
					} else {
						name = fun.(*ast.SelectorExpr).Sel.Name
					}
					if cands := decls[name]; len(cands) == 1 {
						body = cands[0].Body
					}
				}
				if body == nil {
					return true
				}
				if sends, defers := analyze(fset, body); len(sends) > 0 && len(defers) > 0 {
					out = append(out, finding{Pos: fset.Position(g.Pos()), Key: fl.path + "|" + fd.Name.Name + "|" + defers[0], Sends: sends, Defers: defers})
				}
				return true
			})
		}
	}
	slices.SortFunc(out, func(a, b finding) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// readAllow parses an allowlist: one "key # reason" per line; blank lines and lines starting with #
// are comments. A key without a reason is an error: the reason is the point.
func readAllow(r io.Reader) (map[string]string, []string) {
	allow := map[string]string{}
	var bad []string
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, reason, ok := strings.Cut(line, " # ")
		if !ok || strings.TrimSpace(reason) == "" {
			bad = append(bad, fmt.Sprintf("allowlist line %d: %q has no reason after \" # \"", n, key))
			continue
		}
		allow[strings.TrimSpace(key)] = strings.TrimSpace(reason)
	}
	return allow, bad
}

func check(root string, allowFile string, stderr io.Writer) int {
	finds, err := scan(root)
	if err != nil {
		fmt.Fprintf(stderr, "sendcheck: %v\n", err)
		return 2
	}
	allow := map[string]string{}
	if allowFile != "" {
		f, err := os.Open(allowFile)
		if err != nil {
			fmt.Fprintf(stderr, "sendcheck: %v\n", err)
			return 2
		}
		defer f.Close()
		var bad []string
		allow, bad = readAllow(f)
		for _, b := range bad {
			fmt.Fprintln(stderr, "sendcheck: "+b)
		}
		if len(bad) > 0 {
			return 1
		}
	}
	used := map[string]bool{}
	status := 0
	for _, f := range finds {
		if _, ok := allow[f.Key]; ok {
			used[f.Key] = true
			continue
		}
		status = 1
		fmt.Fprintf(stderr, "%s: a goroutine sends or closes %v and then runs the deferred %v: the receiver can act before that bookkeeping is done. "+
			"Do the bookkeeping first (release the slot, clear the flag, decrement) and then send; or, if the order is safe, add %q to the allowlist with the reason.\n",
			f.Pos, f.Sends, f.Defers, f.Key)
	}
	for k := range allow {
		if !used[k] {
			status = 1
			fmt.Fprintf(stderr, "sendcheck: the allowlist entry %q matches no goroutine: delete it\n", k)
		}
	}
	return status
}
