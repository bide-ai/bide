package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/bide-ai/bide/internal/tools/docsnip/snip"
)

// Finding is one problem with the docs, reported as "file:line: message".
type Finding struct {
	File string
	Line int
	Msg  string
}

func (f Finding) String() string { return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Msg) }

// Report is the result of checking a set of blocks.
type Report struct {
	Blocks   int // every go block seen
	Unique   int // distinct blocks (identical setup and code compile once)
	Checked  int // distinct blocks compiled
	Skipped  int // distinct blocks with a skip directive
	Kinds    map[snip.Kind]int
	Status   map[string]string // "file:line" of each block -> how it was compiled, or its skip
	Findings []Finding
}

// Checker type-checks doc blocks against the packages of the Go workspace at Root.
type Checker struct {
	Root string // directory the go command runs in; its go.work or go.mod resolves imports

	fset    *token.FileSet
	names   map[string]string // package name -> import path; "" when the name is ambiguous
	exports map[string]string // import path -> export data file
	loadErr map[string]string // import path -> why it could not be loaded
	imp     types.Importer
	goVer   string
}

// NewChecker lists the packages a block may use without importing them: every package of the
// standard library and of the main module(s) at root, except internal, vendored and main
// packages. When two of them share a name, a package of the main module(s) wins over the
// standard library, and within the standard library the one with the shortest import path
// wins; a name still tied is ambiguous and must be imported in a setup directive.
func NewChecker(root string) (*Checker, error) {
	c := &Checker{
		Root:    root,
		fset:    token.NewFileSet(),
		names:   map[string]string{},
		exports: map[string]string{},
		loadErr: map[string]string{},
	}
	names, goVer, err := snip.PackageNames(root)
	if err != nil {
		return nil, err
	}
	c.names, c.goVer = names, goVer
	c.imp = importer.ForCompiler(c.fset, "gc", func(path string) (io.ReadCloser, error) {
		if f := c.exports[path]; f != "" {
			return os.Open(f)
		}
		if why := c.loadErr[path]; why != "" {
			return nil, errors.New(why)
		}
		return nil, fmt.Errorf("package %s was not loaded", path)
	})
	return c, nil
}

func (c *Checker) goCmd(args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = c.Root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return out, nil
}

// load builds export data for the packages and everything they import, in one go command.
func (c *Checker) load(paths []string) error {
	var need []string
	for _, p := range paths {
		if _, ok := c.exports[p]; !ok && c.loadErr[p] == "" {
			need = append(need, p)
		}
	}
	if len(need) == 0 {
		return nil
	}
	sort.Strings(need)
	out, err := c.goCmd(append([]string{"list", "-e", "-export", "-deps", "-json=ImportPath,Export,Error,DepsErrors"}, need...)...)
	if err != nil {
		return err
	}
	type pkgErr struct{ Err string }
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p struct {
			ImportPath string
			Export     string
			Error      *pkgErr
			DepsErrors []*pkgErr
		}
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("go list output: %v", err)
		}
		switch {
		case p.Error != nil:
			c.loadErr[p.ImportPath] = p.Error.Err
		case p.Export == "":
			why := "no export data"
			if len(p.DepsErrors) > 0 {
				why = p.DepsErrors[0].Err
			}
			c.loadErr[p.ImportPath] = why
		default:
			c.exports[p.ImportPath] = p.Export
		}
	}
	for _, p := range need {
		if _, ok := c.exports[p]; !ok && c.loadErr[p] == "" {
			c.loadErr[p] = "go list did not report the package"
		}
	}
	return nil
}

type entry struct {
	blocks []snip.Block // identical blocks; the first is reported
	setup  snip.Setup
}

// setupFor parses a block's setup or api directive.
func (c *Checker) setupFor(b snip.Block) (snip.Setup, error) {
	if b.Dir == nil {
		return snip.Setup{}, nil
	}
	switch b.Dir.Kind {
	case "setup":
		return snip.ParseSetup(b.Dir.Arg)
	case "api":
		name, items, _ := strings.Cut(b.Dir.Arg, ";")
		name = strings.TrimSpace(name)
		path := name
		if !strings.Contains(path, "/") {
			path = c.names[path]
		}
		if path == "" {
			return snip.Setup{}, fmt.Errorf("api %s: not a known package name or import path", name)
		}
		s, err := snip.ParseSetup(items)
		if err != nil {
			return snip.Setup{}, err
		}
		s.API = path
		s.Imports = append([]string{fmt.Sprintf(". %q", path)}, s.Imports...)
		return s, nil
	}
	return snip.Setup{}, nil
}

// Check type-checks every block and reports what does not compile, and every skip directive
// on a block that compiles.
func (c *Checker) Check(blocks []snip.Block) (*Report, error) {
	r := &Report{Blocks: len(blocks), Kinds: map[snip.Kind]int{}, Status: map[string]string{}}
	var (
		order   []string
		entries = map[string]*entry{}
	)
	for _, b := range blocks {
		h := sha256.New()
		if b.Dir != nil {
			fmt.Fprintf(h, "%s\x00%s\x00", b.Dir.Kind, b.Dir.Arg)
		}
		h.Write([]byte(b.Code))
		key := string(h.Sum(nil))
		if e := entries[key]; e != nil {
			e.blocks = append(e.blocks, b)
			continue
		}
		e := &entry{blocks: []snip.Block{b}}
		entries[key] = e
		setup, err := c.setupFor(b)
		if err != nil {
			r.Findings = append(r.Findings, Finding{b.File, b.Dir.Line, err.Error()})
			continue // not compiled; identical blocks join it and are not reported again
		}
		e.setup = setup
		order = append(order, key)
	}
	r.Unique = len(entries)

	// Load, in one go command, every package the blocks import or may need imported.
	var paths []string
	for _, key := range order {
		e := entries[key]
		u, err := snip.Synthesize(c.fset, e.blocks[0], e.setup, nil)
		if err != nil {
			continue
		}
		paths = append(paths, c.candidates(u.File)...)
	}
	if err := c.load(dedupe(paths)); err != nil {
		return nil, err
	}

	for _, key := range order {
		e := entries[key]
		b := e.blocks[0]
		skip := b.Dir != nil && b.Dir.Kind == "skip"
		kind, errs, err := c.checkBlock(b, e.setup)
		if err != nil {
			return nil, err
		}
		status := string(kind)
		if kind == "" {
			status = "does not parse"
		}
		if skip {
			status = "skipped: " + b.Dir.Arg
		} else if len(errs) > 0 && kind != "" {
			status += ", fails"
		}
		for _, x := range e.blocks {
			r.Status[fmt.Sprintf("%s:%d", x.File, x.Line)] = status
		}
		if skip {
			r.Skipped++
			if len(errs) == 0 {
				r.Findings = append(r.Findings, Finding{b.File, b.Dir.Line, "stale skip: the block compiles; delete the skip directive" + also(e.blocks)})
			}
			continue
		}
		r.Checked++
		if kind != "" {
			r.Kinds[kind]++
		}
		for _, f := range errs {
			f.Msg += also(e.blocks)
			r.Findings = append(r.Findings, f)
		}
	}
	sort.SliceStable(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return r, nil
}

func also(blocks []snip.Block) string {
	if len(blocks) < 2 {
		return ""
	}
	var locs []string
	for _, b := range blocks[1:] {
		locs = append(locs, fmt.Sprintf("%s:%d", b.File, b.Line))
	}
	return " (the same block is also at " + strings.Join(locs, ", ") + ")"
}

func dedupe(s []string) []string {
	sort.Strings(s)
	return slices.Compact(s)
}

// candidates is every import path a file may need: its imports, and the package of every
// known package name used as a selector's operand (a superset of what it needs).
func (c *Checker) candidates(f *ast.File) []string {
	var paths []string
	for _, im := range f.Imports {
		if p := strings.Trim(im.Path.Value, "\"`"); p != "" {
			paths = append(paths, p)
		}
	}
	for name := range selectorOperands(f) {
		if p := c.names[name]; p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

var (
	undefinedName = regexp.MustCompile(`^undefined: ([\p{L}_][\p{L}\p{N}_]*)$`)
	unusedRe      = regexp.MustCompile(`declared and not used|imported and not used|\) is not used$`)
)

// selectorOperands is the set of identifiers used as the operand of a selector (x in x.Sel).
func selectorOperands(f *ast.File) map[string]bool {
	set := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				set[id.Name] = true
			}
		}
		return true
	})
	return set
}

// checkBlock compiles one block. A package name the block uses without importing it is
// imported automatically (see NewChecker). Unused variables and imports are allowed in
// declaration and statement blocks, which are excerpts, as is an expression statement whose
// value is unused (a block listing values); complete files must compile as they are. A block that does not parse, or has a type error, yields findings.
func (c *Checker) checkBlock(b snip.Block, setup snip.Setup) (snip.Kind, []Finding, error) {
	auto := map[string]string{}
	for {
		u, err := snip.Synthesize(c.fset, b, setup, auto)
		if err != nil {
			return "", parseFindings(b, err), nil
		}
		var (
			apiDecls []apiDecl
			apiPkg   *types.Package
			info     = &types.Info{Defs: map[*ast.Ident]types.Object{}}
		)
		if setup.API != "" {
			if u.Kind != snip.KindDecls {
				return u.Kind, []Finding{{b.File, b.Dir.Line, "an api block must hold declarations (functions without bodies, types, vars, consts)"}}, nil
			}
			if apiPkg, err = c.imp.Import(setup.API); err != nil {
				return u.Kind, []Finding{{b.File, b.Dir.Line, fmt.Sprintf("api package %s: %v", setup.API, err)}}, nil
			}
			var bad []string
			u.Kind = snip.KindAPI
			apiDecls, bad = prepareAPI(u.File)
			if len(bad) > 0 {
				return u.Kind, []Finding{{b.File, b.Line, fmt.Sprintf("an api block lists declarations; func %s has a body", strings.Join(bad, ", "))}}, nil
			}
		}
		var terrs []types.Error
		conf := types.Config{
			Importer:  c.imp,
			GoVersion: c.goVer,
			Error:     func(err error) { terrs = append(terrs, err.(types.Error)) },
		}
		conf.Check("docsnip", c.fset, []*ast.File{u.File}, info)

		added := false
		if u.Kind != snip.KindProgram && u.Kind != snip.KindFile {
			qualifiers := selectorOperands(u.File)
			for _, te := range terrs {
				m := undefinedName.FindStringSubmatch(te.Msg)
				if m == nil || !qualifiers[m[1]] {
					continue // only a name used as a qualifier (name.X) is imported
				}
				if p, ok := c.names[m[1]]; ok && p != "" {
					if _, done := auto[m[1]]; !done {
						auto[m[1]] = p
						added = true
						if err := c.load([]string{p}); err != nil {
							return "", nil, err
						}
					}
				}
			}
		}
		if added {
			continue
		}
		var out []Finding
		seen := map[string]bool{}
		for _, te := range terrs {
			pos := c.fset.Position(te.Pos)
			if u.Kind != snip.KindProgram && u.Kind != snip.KindFile {
				if unusedRe.MatchString(te.Msg) {
					continue
				}
				if pos.Filename == snip.WrapperFile && strings.HasPrefix(te.Msg, "missing return") {
					continue // a statement block may end before its function does
				}
				if setup.API != "" && strings.HasSuffix(te.Msg, "missing function body") {
					continue // an api block lists signatures
				}
			}
			msg := te.Msg
			if m := undefinedName.FindStringSubmatch(msg); m != nil {
				if p, ok := c.names[m[1]]; ok && p == "" {
					msg += fmt.Sprintf(" (the package name %s is ambiguous; import it in a setup directive)", m[1])
				}
			}
			f := locate(b, pos, msg)
			if k := f.String(); !seen[k] {
				seen[k] = true
				out = append(out, f)
			}
		}
		if apiPkg == nil {
			// A declaration without a body documents an existing function; it is only checked
			// against the package in an api block.
			for _, d := range u.File.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body == nil {
					if pos := c.fset.Position(fd.Pos()); pos.Filename == b.File && pos.Line >= b.Line {
						out = append(out, Finding{pos.Filename, pos.Line, fmt.Sprintf("func %s has no body: mark the block <!-- docsnip: api package --> to check it against the package, or give it a body", fd.Name.Name)})
					}
				}
			}
		}
		if apiPkg != nil {
			out = append(out, compareAPI(apiPkg, info, apiDecls, func(p token.Pos) Finding {
				return locate(b, c.fset.Position(p), "")
			})...)
		}
		return u.Kind, out, nil
	}
}

func parseFindings(b snip.Block, err error) []Finding {
	var list scanner.ErrorList
	if errors.As(err, &list) {
		var out []Finding
		for _, e := range list {
			out = append(out, locate(b, e.Pos, "syntax error: "+e.Msg))
		}
		return out
	}
	var be *snip.BlockError
	if errors.As(err, &be) {
		return []Finding{{b.File, be.Line, be.Msg}}
	}
	return []Finding{{b.File, b.Line, err.Error()}}
}

// locate maps a position in a synthesized file to the markdown: into the block, onto its
// directive for code from a setup, and onto the block's first line for any other added code.
func locate(b snip.Block, pos token.Position, msg string) Finding {
	switch {
	case pos.Filename == b.File:
		return Finding{b.File, pos.Line, msg}
	case pos.Filename == snip.SetupFile && b.Dir != nil:
		return Finding{b.File, b.Dir.Line, "setup: " + msg}
	}
	return Finding{b.File, b.Line, msg}
}
