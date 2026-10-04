// Package snip reads the Go code blocks of bide's markdown documentation and makes each a Go
// file, which docsnip type-checks.
package snip

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

// WrapperFile names the positions of the code docsnip adds around a statement block, so
// errors there (a missing return at the wrapper's closing brace) can be told apart.
const WrapperFile = "docsnip-wrapper"

// SetupFile names the positions of the imports and declarations docsnip adds from a setup
// directive, so errors there are reported at the directive.
const SetupFile = "docsnip-setup"

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

// Elide replaces the elision forms docs use with code that compiles. Following gofmt's
// spacing, "{ ... }" (or "{...}", "{ /* ... */ }") after a space is a function body and becomes
// a body that panics, and directly after a type name it is a composite literal and becomes
// "{}"; a line holding only "..." becomes a comment.
func Elide(code string) string {
	code = elidedBody.ReplaceAllString(code, ` { panic("docsnip: elided") }`)
	code = elidedLit.ReplaceAllString(code, `$1{}`)
	return elidedLine.ReplaceAllString(code, "$1// ...")
}

// Span is a replacement Elide made: Elided[EStart:EEnd] stands for the original code's
// [OStart:OEnd].
type Span struct{ EStart, EEnd, OStart, OEnd int }

// ElideSpans returns Elide(code) with the replacements it made, in order, so an offset into the
// elided code outside them maps back to the original code.
func ElideSpans(code string) (string, []Span) {
	type rep struct {
		re  *regexp.Regexp
		tpl string
	}
	reps := []rep{{elidedBody, ` { panic("docsnip: elided") }`}, {elidedLit, `$1{}`}, {elidedLine, "$1// ..."}}
	// apply each regexp in turn, composing the offset maps
	var spans []Span // in terms of the original code, kept as the code changes
	cur := code
	for _, r := range reps {
		var b strings.Builder
		var next []Span
		last := 0
		for _, m := range r.re.FindAllStringSubmatchIndex(cur, -1) {
			b.WriteString(cur[last:m[0]])
			start := b.Len()
			b.Write(r.re.ExpandString(nil, r.tpl, cur, m))
			next = append(next, Span{start, b.Len(), m[0], m[1]})
			last = m[1]
		}
		b.WriteString(cur[last:])
		if len(next) > 0 {
			spans = composeSpans(spans, next)
		}
		cur = b.String()
	}
	return cur, spans
}

// composeSpans composes the spans of two successive rewrites: old maps the code before the first
// to the original, next maps the code after the second to the code before it.
func composeSpans(old, next []Span) []Span {
	toOrig := func(off int) int { // an offset of the intermediate code, outside old's spans
		shift := 0
		for _, s := range old {
			if s.EEnd <= off {
				shift = s.OEnd - s.EEnd
			}
		}
		return off + shift
	}
	var out []Span
	for _, n := range next {
		out = append(out, Span{n.EStart, n.EEnd, toOrig(n.OStart), toOrig(n.OEnd)})
	}
	// the old spans shift by the next rewrite's length changes before them
	for _, s := range old {
		shift := 0
		inside := false
		for _, n := range next {
			if n.OEnd <= s.EStart {
				shift = n.EEnd - n.OEnd
			}
			if n.OStart < s.EEnd && n.OEnd > s.EStart {
				inside = true
			}
		}
		if !inside {
			out = append(out, Span{s.EStart + shift, s.EEnd + shift, s.OStart, s.OEnd})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EStart < out[j].EStart })
	return out
}

// BlockError is a problem with a block's directive, at a line of its markdown file.
type BlockError struct {
	Line int
	Msg  string
}

// Error returns the message.
func (e *BlockError) Error() string { return e.Msg }

// Unit is a block made into a Go file.
type Unit struct {
	Kind Kind
	File *ast.File
	// Src is the file's source, and Regions where the block's code is in it: one region, or two
	// for declarations followed by statements. The code there is the block's after Elide.
	Src     string
	Regions []Region
}

// Region is a run of a block's (elided) code in a Unit's source: Src[Start:End] is the code
// from offset Code of the elided code on.
type Region struct{ Start, End, Code int }

// Synthesize turns a block into a parsed Go file. autoImports maps package names the block
// uses without importing to their import paths. A complete file is parsed as written; any
// other block is tried as declarations, then as statements inside a function, then as
// declarations followed by statements, and the parse error that got furthest into the block
// is returned when none parses.
func Synthesize(fset *token.FileSet, b Block, setup Setup, autoImports map[string]string) (*Unit, error) {
	code, _ := ElideSpans(b.Code)
	loc := func(line int) string { return fmt.Sprintf("//line %s:%d\n", b.File, line) }
	if isCompleteFile(code) {
		if b.Dir != nil && b.Dir.Kind != "skip" {
			return nil, &BlockError{b.Dir.Line, "a " + b.Dir.Kind + " directive cannot apply to a complete file (it has a package clause)"}
		}
		src := loc(b.Line) + code
		f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		kind := KindFile
		if f.Name.Name == "main" {
			kind = KindProgram
		}
		return &Unit{Kind: kind, File: f, Src: src, Regions: []Region{{len(src) - len(code), len(src), 0}}}, nil
	}

	var head strings.Builder
	head.WriteString("package docsnip\n")
	head.WriteString("//line " + SetupFile + ":1\n")
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
	tail.WriteString("\n//line " + SetupFile + ":1\n")
	for _, d := range setup.Decls {
		tail.WriteString(d + "\n")
	}

	declSrc := head.String() + loc(b.Line) + code + tail.String()
	df, derr := parser.ParseFile(fset, "", declSrc, parser.ParseComments)
	if derr == nil {
		if setup.Returns != "" {
			return nil, &BlockError{b.Dir.Line, "setup has a returns item, but the block is declarations, not statements"}
		}
		start := len(head.String()) + len(loc(b.Line))
		return &Unit{Kind: KindDecls, File: df, Src: declSrc, Regions: []Region{{start, start + len(code), 0}}}, nil
	}
	results := setup.Returns
	stmtSrc := head.String() + "func _() " + results + " {\n" + loc(b.Line) + code + "\n//line " + WrapperFile + ":1\n}\n" + tail.String()
	sf, serr := parser.ParseFile(fset, "", stmtSrc, parser.ParseComments)
	if serr == nil {
		start := len(head.String()) + len("func _() "+results+" {\n") + len(loc(b.Line))
		return &Unit{Kind: KindStmts, File: sf, Src: stmtSrc, Regions: []Region{{start, start + len(code), 0}}}, nil
	}
	// Declarations (imports, types, funcs) followed by statements: split where the
	// declarations stop parsing, and wrap the rest in the function.
	var list scanner.ErrorList
	if errors.As(derr, &list) && len(list) > 0 && strings.HasPrefix(list[0].Msg, "expected declaration") {
		if split := list[0].Pos.Line - b.Line; split > 0 {
			lines := strings.SplitAfter(code, "\n")
			if split < len(lines) {
				decls, stmts := strings.Join(lines[:split], ""), strings.Join(lines[split:], "")
				mixedSrc := head.String() + loc(b.Line) + decls + "\nfunc _() " + results + " {\n" + loc(b.Line+split) + stmts + "\n//line " + WrapperFile + ":1\n}\n" + tail.String()
				mf, merr := parser.ParseFile(fset, "", mixedSrc, parser.ParseComments)
				if merr == nil {
					d0 := len(head.String()) + len(loc(b.Line))
					s0 := d0 + len(decls) + len("\nfunc _() "+results+" {\n") + len(loc(b.Line+split))
					return &Unit{Kind: KindMixed, File: mf, Src: mixedSrc, Regions: []Region{
						{d0, d0 + len(decls), 0}, {s0, s0 + len(stmts), len(decls)},
					}}, nil
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
