package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strings"
)

// edit replaces src[start:end] with text; start == end is an insertion.
type edit struct {
	start, end int
	text       string
	seq        int // insertion order, which orders insertions at one offset
}

// Editor collects the edits of one file. Rules run on the syntax tree in post-order, so a node's
// rewrite can read its children's rewritten text (Text) and replace them all: an edit that covers
// others absorbs them. Edits that overlap without nesting are a bug in a rule, and panic.
type Editor struct {
	file  *File
	base  int // file offset of the file's first byte in the FileSet
	edits []edit
	seq   int
}

func newEditor(f *File) *Editor {
	tf := f.Pkg.Fset.File(f.AST.Pos())
	return &Editor{file: f, base: tf.Base()}
}

func (e *Editor) off(p token.Pos) int { return int(p) - e.base }

// inside reports whether ed lies within [start, end): a replacement within it, or an insertion
// strictly inside it (an insertion at either end belongs to the code around the range).
func inside(ed edit, start, end int) bool {
	if ed.start == ed.end {
		return ed.start > start && ed.start < end
	}
	return ed.start >= start && ed.end <= end
}

// Replace replaces node n's source with text, which a rule builds from its children's Text.
func (e *Editor) Replace(n ast.Node, text string) { e.ReplaceRange(n.Pos(), n.End(), text) }

// ReplaceRange replaces the source from start to end.
func (e *Editor) ReplaceRange(start, end token.Pos, text string) {
	s, t := e.off(start), e.off(end)
	kept := e.edits[:0]
	for _, ed := range e.edits {
		switch {
		case inside(ed, s, t):
			// absorbed: the caller built text from Text, which applied it
		case ed.start == ed.end || ed.end <= s || ed.start >= t:
			kept = append(kept, ed)
		default:
			panic(fmt.Sprintf("%s: overlapping edits [%d,%d) and [%d,%d)", e.file.Path, ed.start, ed.end, s, t))
		}
	}
	e.edits = append(kept, edit{s, t, text, e.next()})
}

// InsertBefore inserts text before node n (outside it).
func (e *Editor) InsertBefore(n ast.Node, text string) { e.insert(e.off(n.Pos()), text) }

// InsertAfter inserts text after node n (outside it).
func (e *Editor) InsertAfter(n ast.Node, text string) { e.insert(e.off(n.End()), text) }

// InsertAt inserts text at p.
func (e *Editor) InsertAt(p token.Pos, text string) { e.insert(e.off(p), text) }

func (e *Editor) insert(at int, text string) {
	e.edits = append(e.edits, edit{at, at, text, e.next()})
}

func (e *Editor) next() int { e.seq++; return e.seq }

// Text returns n's source with the edits inside it applied.
func (e *Editor) Text(n ast.Node) string { return e.TextRange(n.Pos(), n.End()) }

// TextRange returns the source from start to end with the edits inside it applied.
func (e *Editor) TextRange(start, end token.Pos) string {
	s, t := e.off(start), e.off(end)
	var in []edit
	for _, ed := range e.edits {
		if inside(ed, s, t) {
			in = append(in, ed)
		}
	}
	return apply(e.file.Src[s:t], s, in)
}

// Orig returns n's original source.
func (e *Editor) Orig(n ast.Node) string { return string(e.file.Src[e.off(n.Pos()):e.off(n.End())]) }

// Changed reports whether the file has edits.
func (e *Editor) Changed() bool { return len(e.edits) > 0 }

// Apply returns the file's source with every edit applied.
func (e *Editor) Apply() []byte { return []byte(apply(e.file.Src, 0, e.edits)) }

// apply applies edits (offsets relative to the file) to src, which starts at file offset base.
func apply(src []byte, base int, edits []edit) string {
	edits = append([]edit(nil), edits...)
	sort.SliceStable(edits, func(i, j int) bool {
		a, b := edits[i], edits[j]
		if a.start != b.start {
			return a.start < b.start
		}
		// at one offset, insertions first (in order), then the replacement
		if (a.start == a.end) != (b.start == b.end) {
			return a.start == a.end
		}
		return a.seq < b.seq
	})
	var b strings.Builder
	pos := base
	for _, ed := range edits {
		if ed.start < pos {
			panic(fmt.Sprintf("overlapping edits at offset %d", ed.start))
		}
		b.Write(src[pos-base : ed.start-base])
		b.WriteString(ed.text)
		pos = ed.end
	}
	b.Write(src[pos-base:])
	return b.String()
}

// DeleteArg deletes the i-th argument of call with its separating comma, keeping the layout of
// the others.
func (e *Editor) DeleteArg(call *ast.CallExpr, i int) {
	switch {
	case i > 0: // with the separator before it, so the line breaks after the others stay
		e.ReplaceRange(call.Args[i-1].End(), call.Args[i].End(), "")
	case i+1 < len(call.Args):
		e.ReplaceRange(call.Args[i].Pos(), call.Args[i+1].Pos(), "")
	default:
		e.Replace(call.Args[i], "")
	}
}

// AppendArgs adds args after call's last argument (before a trailing comma), keeping its layout.
func (e *Editor) AppendArgs(call *ast.CallExpr, args ...string) {
	if len(args) == 0 {
		return
	}
	text := strings.Join(args, ", ")
	if n := len(call.Args); n > 0 {
		e.InsertAfter(call.Args[n-1], ", "+text)
		return
	}
	e.InsertAt(call.Rparen, text)
}

// DeleteStmt deletes a statement with the indentation before it and the newline after it.
func (e *Editor) DeleteStmt(s ast.Stmt) {
	src := e.file.Src
	start, end := e.off(s.Pos()), e.off(s.End())
	for start > 0 && (src[start-1] == '\t' || src[start-1] == ' ') {
		start--
	}
	if end < len(src) && src[end] == '\n' {
		end++
	}
	e.ReplaceRange(token.Pos(start+e.base), token.Pos(end+e.base), "")
}
