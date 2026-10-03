package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// MigrateModule runs rules over the packages patterns match in the module at dir, and returns
// each changed file's new source, keyed by path, with what the run found. Each rule is its own
// pass over the code the previous ones left, loaded and type-checked again, so the rewrites of one
// class never meet another's half-done (Rules gives the order). The files are not written: each
// pass reads the previous passes' output through an overlay.
func MigrateModule(dir string, patterns []string, rules []Rule) (map[string][]byte, *Run, error) {
	out := map[string][]byte{}
	res := &Run{Counts: map[string]int{}}
	type passFindings struct {
		fs     []Finding
		before map[string][]byte // the files as the pass read them
	}
	var passes []passFindings
	for _, r := range rules {
		pkgs, err := Load(dir, patterns, out)
		if err != nil {
			return nil, nil, err
		}
		before := map[string][]byte{}
		for _, p := range pkgs {
			for _, f := range p.Files {
				before[f.Path] = f.Src
			}
		}
		o, rr, err := Migrate(pkgs, []Rule{r})
		if err != nil {
			return nil, nil, err
		}
		for p, b := range o {
			out[p] = b
		}
		for k, v := range rr.Counts {
			res.Counts[k] += v
		}
		passes = append(passes, passFindings{rr.Findings, before})
	}
	// Each pass found its sites in the files as it read them: name them in the files on disk,
	// as the run found them.
	for _, p := range passes {
		remapFindings(p.fs, p.before, nil)
		res.Findings = append(res.Findings, p.fs...)
	}
	return out, res, nil
}

// Migrate runs rules over every file of pkgs and returns each changed file's new source, keyed by
// path, with what the run found.
func Migrate(pkgs []*Package, rules []Rule) (map[string][]byte, *Run, error) {
	run := &Run{Counts: map[string]int{}}
	out := map[string][]byte{}
	for _, p := range pkgs {
		for _, f := range p.Files {
			if _, done := out[f.Path]; done {
				continue
			}
			src, err := migrateFile(run, f, rules)
			if err != nil {
				return nil, nil, err
			}
			if src != nil {
				out[f.Path] = src
			}
		}
	}
	sort.Slice(run.Findings, func(i, j int) bool {
		a, b := run.Findings[i].Pos, run.Findings[j].Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Offset < b.Offset
	})
	return out, run, nil
}

// migrateFile applies rules to f, and returns its new source, or nil if nothing changed.
func migrateFile(run *Run, f *File, rules []Rule) (src []byte, err error) {
	if f.Pkg.Path == agentPath && f.Pkg.Name == "agent" && !strings.HasSuffix(f.Path, "_test.go") {
		return nil, nil // package agent's own API is migrated by hand
	}
	c, err := rewrite(run, f, rules)
	if err != nil {
		return nil, err
	}
	if !c.Ed.Changed() && len(c.added) == 0 {
		return nil, nil
	}
	c.addImports()
	b := c.Ed.Apply()
	b, err = tidyImports(f.Path, b)
	if err != nil {
		return nil, err
	}
	fb, err := format.Source(b)
	if err != nil {
		return nil, fmt.Errorf("%s: the rewritten file does not parse: %v\n%s", f.Path, err, b)
	}
	if bytes.Equal(fb, f.Src) || bytes.Equal(fb, bytes.ReplaceAll(f.Src, []byte("\r\n"), []byte("\n"))) {
		return nil, nil // unchanged (gofmt writes LF: a CRLF file that only differs in that is too)
	}
	return fb, nil
}

// rewrite runs rules over f and returns the context holding the edits they made.
func rewrite(run *Run, f *File, rules []Rule) (c *Ctx, err error) {
	c = newCtx(run, f)
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s: %v", f.Path, r)
		}
	}()
	walk := func(visit func(r Rule, n ast.Node)) {
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if n == nil {
				top := c.stack[len(c.stack)-1]
				for _, r := range rules {
					c.rule = r.Name
					visit(r, top)
				}
				c.stack = c.stack[:len(c.stack)-1]
				return true
			}
			c.stack = append(c.stack, n)
			return true
		})
	}
	walk(func(r Rule, n ast.Node) {
		if r.Plan != nil {
			r.Plan(c, n)
		}
	})
	for _, r := range rules {
		if r.Planned != nil {
			c.rule = r.Name
			r.Planned(c)
		}
	}
	walk(func(r Rule, n ast.Node) { r.Visit(c, n) })
	return c, nil
}

// addImports adds the imports rules asked for to the file's import declaration.
func (c *Ctx) addImports() {
	if len(c.added) == 0 {
		return
	}
	paths := make([]string, 0, len(c.added))
	for p := range c.added {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var lines []string
	for _, p := range paths {
		n := c.added[p]
		if n == pkgName(p) {
			lines = append(lines, strconv.Quote(p))
		} else {
			lines = append(lines, n+" "+strconv.Quote(p))
		}
	}
	for _, d := range c.File.AST.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		if gd.Lparen.IsValid() {
			// each into the group of its kind: the standard library's (no dot in its first
			// element) or the others'
			for _, l := range lines {
				std := !strings.Contains(strings.SplitN(strings.Trim(l[strings.Index(l, "\"")+1:], "\""), "/", 2)[0], ".")
				var after ast.Node
				for _, s := range gd.Specs {
					p, _ := strconv.Unquote(s.(*ast.ImportSpec).Path.Value)
					if !strings.Contains(strings.SplitN(p, "/", 2)[0], ".") == std {
						after = s
					}
				}
				if after != nil {
					c.Ed.InsertAfter(after, "\n"+l)
				} else {
					c.Ed.InsertAt(gd.Rparen, l+"\n")
				}
			}
			return
		}
	}
	text := "\n\nimport (\n" + strings.Join(lines, "\n") + "\n)\n"
	if n := len(c.File.AST.Imports); n > 0 {
		// after the last single import declaration
		var last ast.Node
		for _, d := range c.File.AST.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
				last = gd
			}
		}
		c.Ed.InsertAfter(last, text)
		return
	}
	c.Ed.InsertAfter(c.File.AST.Name, text)
}

// tidyImports removes the imports of src that nothing uses any more: a rewrite may leave a
// package unused (agent, once only agenttest is called). Blank and dot imports are kept.
func tidyImports(path string, src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("%s: the rewritten file does not parse: %v\n%s", path, err, src)
	}
	used := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				used[id.Name] = true
			}
		}
		return true
	})
	type cut struct{ start, end int }
	var cuts []cut
	base := fset.File(f.Pos()).Base()
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		var dead []*ast.ImportSpec
		for _, s := range gd.Specs {
			is := s.(*ast.ImportSpec)
			p, _ := strconv.Unquote(is.Path.Value)
			name := pkgName(p)
			if is.Name != nil {
				name = is.Name.Name
			}
			if name == "_" || name == "." || used[name] || !strings.HasPrefix(p, bidePath) && p != "log" {
				continue
			}
			if is.Name == nil && !knownName(p) {
				continue // the package's name may differ from its path; leave it
			}
			dead = append(dead, is)
		}
		if len(dead) == 0 {
			continue
		}
		if len(dead) == len(gd.Specs) {
			cuts = append(cuts, cut{int(gd.Pos()) - base, int(gd.End()) - base})
			continue
		}
		for _, is := range dead {
			cuts = append(cuts, cut{int(is.Pos()) - base, int(is.End()) - base})
		}
	}
	if len(cuts) == 0 {
		return src, nil
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].start < cuts[j].start })
	var b strings.Builder
	pos := 0
	for _, ct := range cuts {
		b.Write(src[pos:ct.start])
		pos = ct.end
	}
	b.Write(src[pos:])
	return []byte(b.String()), nil
}

// knownName reports whether an import path's package name is its last element, as for every
// bide package and the standard library packages the rules emit.
func knownName(path string) bool {
	return strings.HasPrefix(path, bidePath) && !strings.Contains(path, "/v") || path == "log"
}
