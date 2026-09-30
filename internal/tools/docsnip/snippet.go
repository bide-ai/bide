package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"regexp"
	"sort"
	"strings"
)

// Kind is how a block was compiled.
type Kind string

// The kinds of block.
const (
	KindProgram Kind = "program"      // a complete file of package main
	KindFile    Kind = "file"         // a complete file of another package
	KindDecls   Kind = "declarations" // top-level declarations, compiled as a package
	KindStmts   Kind = "statements"   // statements, compiled as the body of a function
	KindMixed   Kind = "declarations and statements"
	KindAPI     Kind = "api" // declarations checked against a package's own (see api.go)
)

// wrapperFile names the positions of the code docsnip adds around a statement block, so
// errors there (a missing return at the wrapper's closing brace) can be told apart.
const wrapperFile = "docsnip-wrapper"

// setupFile names the positions of the imports and declarations docsnip adds from a setup
// directive, so errors there are reported at the directive.
const setupFile = "docsnip-setup"

// Setup is a parsed setup directive.
type Setup struct {
	Imports []string // import specs: `"path"` or `name "path"`
	Decls   []string // top-level declarations
	Returns string   // result list of the function wrapping a statement block
	API     string   // import path of the package an api block documents
}

var declKeyword = regexp.MustCompile(`^(type|func|var|const)\b`)

// ParseSetup parses the items of a setup directive. Items are separated by semicolons or
// newlines outside brackets and string literals. An item is one of:
//
//	import "path"           an import (also: import name "path")
//	returns T               the results of the function that wraps a statement block
//	type/func/var/const ... a top-level declaration, used as written
//	name[, name] T          shorthand for "var name[, name] T"
func ParseSetup(arg string) (Setup, error) {
	var s Setup
	for _, item := range splitItems(arg) {
		switch {
		case strings.HasPrefix(item, "import ") || strings.HasPrefix(item, "import\t"):
			s.Imports = append(s.Imports, strings.TrimSpace(item[len("import"):]))
		case strings.HasPrefix(item, "returns ") || strings.HasPrefix(item, "returns\t"):
			if s.Returns != "" {
				return s, errors.New("setup has more than one returns item")
			}
			s.Returns = strings.TrimSpace(item[len("returns"):])
		case declKeyword.MatchString(item):
			s.Decls = append(s.Decls, item)
		default:
			s.Decls = append(s.Decls, "var "+item)
		}
	}
	return s, nil
}

// splitItems splits s at semicolons and newlines that are outside brackets, string literals
// and rune literals.
func splitItems(s string) []string {
	var (
		items []string
		depth int
		start int
		quote byte
	)
	flush := func(end int) {
		if it := strings.TrimSpace(s[start:end]); it != "" {
			items = append(items, it)
		}
		start = end + 1
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote != '`' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '`', '\'':
			quote = c
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
		case ';', '\n':
			if depth == 0 {
				flush(i)
			}
		}
	}
	flush(len(s))
	return items
}

var (
	elidedBody = regexp.MustCompile(` \{ ?(\.\.\.|/\* \.\.\. \*/) ?\}`)
	elidedLit  = regexp.MustCompile(`([\p{L}\p{N}_\]])\{ ?(\.\.\.|/\* \.\.\. \*/) ?\}`)
	elidedLine = regexp.MustCompile(`(?m)^([ \t]*)\.\.\.[ \t]*$`)
)

// elide replaces the elision forms docs use with code that compiles. Following gofmt's
// spacing, "{ ... }" (or "{...}", "{ /* ... */ }") after a space is a function body and becomes
// a body that panics, and directly after a type name it is a composite literal and becomes
// "{}"; a line holding only "..." becomes a comment.
func elide(code string) string {
	code = elidedBody.ReplaceAllString(code, ` { panic("docsnip: elided") }`)
	code = elidedLit.ReplaceAllString(code, `$1{}`)
	return elidedLine.ReplaceAllString(code, "$1// ...")
}

// BlockError is a problem with a block's directive, at a line of its markdown file.
type BlockError struct {
	Line int
	Msg  string
}

func (e *BlockError) Error() string { return e.Msg }

// locate maps a position in a synthesized file to the markdown: into the block, onto its
// directive for code from a setup, and onto the block's first line for any other added code.
func (b Block) locate(pos token.Position, msg string) Finding {
	switch {
	case pos.Filename == b.File:
		return Finding{b.File, pos.Line, msg}
	case pos.Filename == setupFile && b.Dir != nil:
		return Finding{b.File, b.Dir.Line, "setup: " + msg}
	}
	return Finding{b.File, b.Line, msg}
}

// Unit is a block made into a Go file.
type Unit struct {
	Kind Kind
	File *ast.File
}

// Synthesize turns a block into a parsed Go file. autoImports maps package names the block
// uses without importing to their import paths. A complete file is parsed as written; any
// other block is tried as declarations, then as statements inside a function, then as
// declarations followed by statements, and the parse error that got furthest into the block
// is returned when none parses.
func Synthesize(fset *token.FileSet, b Block, setup Setup, autoImports map[string]string) (*Unit, error) {
	code := elide(b.Code)
	loc := func(line int) string { return fmt.Sprintf("//line %s:%d\n", b.File, line) }
	if isCompleteFile(code) {
		if b.Dir != nil && b.Dir.Kind != "skip" {
			return nil, &BlockError{b.Dir.Line, "a " + b.Dir.Kind + " directive cannot apply to a complete file (it has a package clause)"}
		}
		f, err := parser.ParseFile(fset, "", loc(b.Line)+code, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		kind := KindFile
		if f.Name.Name == "main" {
			kind = KindProgram
		}
		return &Unit{Kind: kind, File: f}, nil
	}

	var head strings.Builder
	head.WriteString("package docsnip\n")
	head.WriteString("//line " + setupFile + ":1\n")
	for _, spec := range setup.Imports {
		head.WriteString("import " + spec + "\n")
	}
	names := make([]string, 0, len(autoImports))
	for n := range autoImports {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&head, "import %s %q\n", n, autoImports[n])
	}
	var tail strings.Builder
	tail.WriteString("\n//line " + setupFile + ":1\n")
	for _, d := range setup.Decls {
		tail.WriteString(d + "\n")
	}

	declSrc := head.String() + loc(b.Line) + code + tail.String()
	df, derr := parser.ParseFile(fset, "", declSrc, parser.ParseComments)
	if derr == nil {
		if setup.Returns != "" {
			return nil, &BlockError{b.Dir.Line, "setup has a returns item, but the block is declarations, not statements"}
		}
		return &Unit{Kind: KindDecls, File: df}, nil
	}
	results := setup.Returns
	stmtSrc := head.String() + "func _() " + results + " {\n" + loc(b.Line) + code + "\n//line " + wrapperFile + ":1\n}\n" + tail.String()
	sf, serr := parser.ParseFile(fset, "", stmtSrc, parser.ParseComments)
	if serr == nil {
		return &Unit{Kind: KindStmts, File: sf}, nil
	}
	// Declarations (imports, types, funcs) followed by statements: split where the
	// declarations stop parsing, and wrap the rest in the function.
	var list scanner.ErrorList
	if errors.As(derr, &list) && len(list) > 0 && strings.HasPrefix(list[0].Msg, "expected declaration") {
		if split := list[0].Pos.Line - b.Line; split > 0 {
			lines := strings.SplitAfter(code, "\n")
			if split < len(lines) {
				decls, stmts := strings.Join(lines[:split], ""), strings.Join(lines[split:], "")
				mixedSrc := head.String() + loc(b.Line) + decls + "\nfunc _() " + results + " {\n" + loc(b.Line+split) + stmts + "\n//line " + wrapperFile + ":1\n}\n" + tail.String()
				mf, merr := parser.ParseFile(fset, "", mixedSrc, parser.ParseComments)
				if merr == nil {
					return &Unit{Kind: KindMixed, File: mf}, nil
				}
				if errPos(fset, merr) > errPos(fset, serr) && errPos(fset, merr) > errPos(fset, derr) {
					return nil, merr
				}
			}
		}
	}
	if errPos(fset, serr) >= errPos(fset, derr) {
		return nil, serr
	}
	return nil, derr
}

// errPos is the line of a parse error within its block, for comparing how far two parses got.
func errPos(_ *token.FileSet, err error) int {
	var list scanner.ErrorList
	if errors.As(err, &list) && len(list) > 0 {
		return list[0].Pos.Line
	}
	return 0
}

var packageClause = regexp.MustCompile(`^package\s+[\p{L}_][\p{L}\p{N}_]*\s*(//.*)?$`)

// isCompleteFile reports whether the first line of code outside comments is a package clause.
func isCompleteFile(code string) bool {
	inBlock := false
	for _, line := range strings.Split(code, "\n") {
		t := strings.TrimSpace(line)
		if inBlock {
			if i := strings.Index(t, "*/"); i >= 0 {
				inBlock = false
				t = strings.TrimSpace(t[i+2:])
			} else {
				continue
			}
		}
		if strings.HasPrefix(t, "/*") && !strings.Contains(t, "*/") {
			inBlock = true
			continue
		}
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		return packageClause.MatchString(t)
	}
	return false
}
