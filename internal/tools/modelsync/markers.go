package main

import (
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// region is one marked region of Go code: the lines from its begin marker to its end marker,
// both included.
type region struct {
	file       string // slash-separated, relative to the root
	model      string
	actions    []string
	begin, end int
}

var (
	// A comment that is meant as a marker: "protocol:" directly followed by a name.
	candidateRE = regexp.MustCompile(`^//\s*protocol:[A-Za-z0-9_]`)
	// A well-formed marker.
	markerRE = regexp.MustCompile(`^// protocol:([a-z0-9_-]+) (begin|end)((?: [A-Za-z_][A-Za-z0-9_]*)*)\s*$`)
)

// problem is a malformed marker or a broken pair.
type problem struct {
	line int
	msg  string
}

// scanRegions returns the marked regions of a Go source file and the problems with its markers.
// Only line comments are read (go/scanner), so a marker inside a string literal is not one.
// Regions of one model may not nest; regions of different models may overlap.
func scanRegions(file string, src []byte, models map[string]*model) ([]region, []problem) {
	fset := token.NewFileSet()
	tf := fset.AddFile(file, -1, len(src))
	var s scanner.Scanner
	s.Init(tf, src, nil, scanner.ScanComments)
	var regions []region
	var probs []problem
	open := map[string]*region{}
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.COMMENT || !candidateRE.MatchString(lit) {
			continue
		}
		line := fset.Position(pos).Line
		m := markerRE.FindStringSubmatch(lit)
		if m == nil {
			probs = append(probs, problem{line, fmt.Sprintf("malformed marker %q: want \"// protocol:<model> begin <Action> ...\" or \"// protocol:<model> end\"", lit)})
			continue
		}
		name, kind, acts := m[1], m[2], strings.Fields(m[3])
		if models[name] == nil {
			probs = append(probs, problem{line, fmt.Sprintf("marker names model %q, which is not a model directory under spec/tla", name)})
			continue
		}
		switch kind {
		case "begin":
			if len(acts) == 0 {
				probs = append(probs, problem{line, fmt.Sprintf("begin marker of %s names no action", name)})
			}
			if o := open[name]; o != nil {
				probs = append(probs, problem{line, fmt.Sprintf("begin marker of %s inside the %s region opened at line %d (regions of one model do not nest)", name, name, o.begin)})
				continue
			}
			open[name] = &region{file: file, model: name, actions: acts, begin: line}
		case "end":
			if len(acts) != 0 {
				probs = append(probs, problem{line, fmt.Sprintf("end marker of %s names actions; only the begin marker does", name)})
			}
			o := open[name]
			if o == nil {
				probs = append(probs, problem{line, fmt.Sprintf("end marker of %s with no open region", name)})
				continue
			}
			o.end = line
			regions = append(regions, *o)
			delete(open, name)
		}
	}
	for _, name := range sortedKeys(open) {
		probs = append(probs, problem{open[name].begin, fmt.Sprintf("begin marker of %s is never closed", name)})
	}
	return regions, probs
}

// scanTree scans every Go file under root, skipping version control, vendor and testdata
// directories, and reports marker problems as findings.
func scanTree(root string, models map[string]*model, r *report) ([]region, error) {
	var all []region
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != root && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata" || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		regions, probs := scanRegions(rel, src, models)
		for _, p := range probs {
			r.errorf("%s:%d: %s", rel, p.line, p.msg)
		}
		all = append(all, regions...)
		return nil
	})
	return all, err
}
