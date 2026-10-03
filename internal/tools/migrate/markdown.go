package main

import (
	"fmt"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bide-ai/bide/internal/tools/docsnip/snip"
)

// MigrateMarkdown rewrites the Go code blocks of the markdown files mdFiles (paths under root, the
// module the blocks compile in) with rules, and returns each changed file's new content.
//
// Each block is made into a Go file the way docsnip compiles it (a complete file, declarations,
// or statements wrapped in a function, with its setup directive's declarations and the packages it
// names imported), written into a temporary package inside the module, loaded and type-checked
// with the module's packages, and rewritten like any Go file; the edits that fall inside the
// block's own code are then applied to the block in the markdown, re-indented. A block with a skip
// or api directive is left alone (api blocks list a package's declarations, and are updated with
// the package). An edit that touches a docs elision ("{ ... }", "...") is not applied, and the
// block is reported. Setup directives' declarations are rewritten by name (see setupNames).
func MigrateMarkdown(root string, mdFiles []string, rules []Rule) (map[string][]byte, *Run, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	names, _, err := snip.PackageNames(root)
	if err != nil {
		return nil, nil, err
	}
	if err := addBideNames(root, names); err != nil {
		return nil, nil, err
	}
	content := map[string]string{}
	for _, f := range mdFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		content[f] = string(b)
	}
	res := &Run{Counts: map[string]int{}}
	type passFindings struct {
		from, to int               // the pass's findings: res.Findings[from:to]
		before   map[string][]byte // the files as the pass read them
	}
	var passes []passFindings
	for _, r := range rules {
		before := map[string][]byte{}
		for f, c := range content {
			before[f] = []byte(c)
		}
		from := len(res.Findings)
		if err := markdownPass(root, names, content, r, res); err != nil {
			return nil, nil, err
		}
		passes = append(passes, passFindings{from, len(res.Findings), before})
	}
	out := map[string][]byte{}
	final := map[string][]byte{}
	for _, f := range mdFiles {
		c := rewriteSetups(content[f])
		final[f] = []byte(c)
		if orig, _ := os.ReadFile(f); string(orig) != c {
			out[f] = []byte(c)
		}
	}
	// each pass's findings name lines of the files as it read them: name them in the result
	for _, p := range passes {
		remapFindings(res.Findings[p.from:p.to], p.before, final)
	}
	return out, res, nil
}

// addBideNames adds to names the packages of the bide modules the module at root requires, under
// their package names (agent, audit, ...), where the module has no package of that name itself:
// snip.PackageNames lists only the module's own packages and the standard library, so a user's
// blocks naming agent would resolve nothing.
func addBideNames(root string, names map[string]string) error {
	m, err := readGoMod(root)
	if err != nil {
		return err
	}
	var patterns []string
	for _, r := range m.Require {
		if r.Path == bidePath || strings.HasPrefix(r.Path, bidePath+"/") {
			patterns = append(patterns, r.Path+"/...")
		}
	}
	if len(patterns) == 0 {
		return nil
	}
	cmd := exec.Command("go", append([]string{"list", "-e", "-f", "{{.ImportPath}} {{.Name}}"}, patterns...)...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list %s: %v", strings.Join(patterns, " "), err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, name, ok := strings.Cut(line, " ")
		if !ok || name == "main" || !publicPath(path) {
			continue
		}
		if _, taken := names[name]; !taken {
			names[name] = path
		}
	}
	return nil
}

// publicPath reports whether a package path is one a user imports: none of internal, testdata,
// cmd or examples.
func publicPath(path string) bool {
	for _, el := range strings.Split(path, "/") {
		if el == "internal" || el == "testdata" || el == "cmd" || el == "examples" {
			return false
		}
	}
	return true
}

// snipUnit is one block made into a Go file.
type snipUnit struct {
	md    string
	block snip.Block
	unit  *snip.Unit
	spans []snip.Span
	path  string // the Go file
}

func markdownPass(root string, names map[string]string, content map[string]string, rule Rule, res *Run) error {
	tmp, err := os.MkdirTemp(root, "migratesnip")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	fset := token.NewFileSet()
	var units []*snipUnit
	files := make([]string, 0, len(content))
	for f := range content {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		blocks, err := snip.Extract(f, []byte(content[f]))
		if err != nil {
			return err
		}
		for _, b := range blocks {
			var setup snip.Setup
			if b.Dir != nil {
				if b.Dir.Kind != "setup" {
					continue
				}
				if setup, err = snip.ParseSetup(b.Dir.Arg); err != nil {
					continue // docsnip reports it
				}
			}
			elided, spans := snip.ElideSpans(b.Code)
			u, err := snip.Synthesize(fset, b, setup, autoImports(elided+"\n"+strings.Join(setup.Decls, "\n"), names, setup))
			if err != nil {
				continue // a block that does not parse: docsnip reports it
			}
			dir := filepath.Join(tmp, fmt.Sprintf("b%d", len(units)))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			path := filepath.Join(dir, "snip.go")
			if err := os.WriteFile(path, []byte(u.Src), 0o644); err != nil {
				return err
			}
			units = append(units, &snipUnit{md: f, block: b, unit: u, spans: spans, path: path})
		}
	}
	if len(units) == 0 {
		return nil
	}
	pkgs, err := Load(root, []string{"./" + filepath.Base(tmp) + "/..."}, nil)
	if err != nil {
		return err
	}
	byPath := map[string]*File{}
	for _, p := range pkgs {
		if os.Getenv("MIGRATE_DEBUG") != "" {
			fmt.Fprintln(os.Stderr, "pkg", p.ID, len(p.Files), p.TypeErrors)
		}
		for _, f := range p.Files {
			byPath[f.Path] = f
		}
	}
	// rewrite each block, then apply each file's blocks from the last up, so line numbers hold
	newCode := map[*snipUnit]string{}
	for _, u := range units {
		f := byPath[u.path]
		if f == nil {
			continue
		}
		run := &Run{Counts: map[string]int{}, Docs: true}
		c, err := rewrite(run, f, []Rule{rule})
		if err != nil {
			return err
		}
		for k, v := range run.Counts {
			res.Counts[k] += v
		}
		for _, fd := range run.Findings {
			fd.Pos = token.Position{Filename: u.md, Line: fd.Pos.Line}
			res.Findings = append(res.Findings, fd)
		}
		code, ok, why := mapEdits(c.Ed.edits, u)
		if !ok {
			res.Findings = append(res.Findings, Finding{Pos: token.Position{Filename: u.md, Line: u.block.Line}, Rule: rule.Name, Msg: why})
			continue
		}
		if code != u.block.Code {
			newCode[u] = code
		}
	}
	sort.Slice(units, func(i, j int) bool {
		if units[i].md != units[j].md {
			return units[i].md < units[j].md
		}
		return units[i].block.Line > units[j].block.Line
	})
	for _, u := range units {
		code, ok := newCode[u]
		if !ok {
			continue
		}
		lines := strings.Split(content[u.md], "\n")
		var repl []string
		for _, l := range strings.Split(strings.TrimSuffix(code, "\n"), "\n") {
			if l != "" {
				l = u.block.Indent + l
			}
			repl = append(repl, l)
		}
		first := u.block.Line - 1
		lines = append(lines[:first], append(repl, lines[first+u.block.Lines:]...)...)
		content[u.md] = strings.Join(lines, "\n")
	}
	return nil
}

// selectorOperand finds identifiers used as a selector's operand (x in x.Sel).
var selectorOperand = regexp.MustCompile(`(?:^|[^\w.])([\p{L}_][\p{L}\p{N}_]*)\.[\p{L}_]`)

// autoImports maps each package name the code uses as a qualifier, and does not import in its
// setup, to its import path.
func autoImports(code string, names map[string]string, setup snip.Setup) map[string]string {
	have := map[string]bool{}
	for _, im := range setup.Imports {
		f := strings.Fields(im)
		path := strings.Trim(f[len(f)-1], "\"`")
		name := filepath.Base(path)
		if len(f) == 2 {
			name = f[0]
		}
		have[name] = true
	}
	auto := map[string]string{}
	for _, m := range selectorOperand.FindAllStringSubmatch(code, -1) {
		n := m[1]
		if p := names[n]; p != "" && !have[n] && !strings.Contains(code, strconv.Quote(p)) {
			auto[n] = p
		}
	}
	return auto
}

// mapEdits applies the edits that fall inside u's code regions to the block's original code, and
// returns it. An edit that crosses a region's edge or touches an elision is refused.
func mapEdits(edits []edit, u *snipUnit) (string, bool, string) {
	type medit struct {
		start, end int
		text       string
		seq        int
	}
	var ms []medit
	for _, ed := range edits {
		var reg *snip.Region
		for i := range u.unit.Regions {
			r := &u.unit.Regions[i]
			if ed.start >= r.Start && ed.end <= r.End {
				reg = r
				break
			}
		}
		if reg == nil {
			for _, r := range u.unit.Regions {
				if ed.start < r.End && ed.end > r.Start {
					return "", false, "a rewrite crosses the edge of the block's code: rewrite it by hand"
				}
			}
			continue // in the code the block was wrapped in (an import it gets by name)
		}
		s, ok1 := toOrig(ed.start-reg.Start+reg.Code, u.spans, ed.start == ed.end)
		e, ok2 := toOrig(ed.end-reg.Start+reg.Code, u.spans, true)
		if !ok1 || !ok2 {
			return "", false, "a rewrite touches an elision (\"...\"): rewrite the block by hand"
		}
		ms = append(ms, medit{s, e, ed.text, ed.seq})
	}
	if len(ms) == 0 {
		return u.block.Code, true, ""
	}
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].start != ms[j].start {
			return ms[i].start < ms[j].start
		}
		if (ms[i].start == ms[i].end) != (ms[j].start == ms[j].end) {
			return ms[i].start == ms[i].end
		}
		return ms[i].seq < ms[j].seq
	})
	code := u.block.Code
	var b strings.Builder
	pos := 0
	for _, m := range ms {
		if m.start < pos {
			return "", false, "overlapping rewrites: rewrite the block by hand"
		}
		b.WriteString(code[pos:m.start])
		b.WriteString(reindent(m.text, lineIndent(code, m.start)))
		pos = m.end
	}
	b.WriteString(code[pos:])
	return b.String(), true, ""
}

// toOrig maps an offset of the elided code to the original code, or reports that it falls inside
// an elision. An offset at an elision's end maps to the original's end of it when end is true.
func toOrig(off int, spans []snip.Span, end bool) (int, bool) {
	shift := 0
	for _, s := range spans {
		switch {
		case off <= s.EStart:
			return off + shift, true
		case off >= s.EEnd:
			shift = s.OEnd - s.EEnd
		default:
			return 0, false
		}
	}
	return off + shift, true
}

// lineIndent returns the leading whitespace of the line of code holding offset at.
func lineIndent(code string, at int) string {
	start := strings.LastIndex(code[:at], "\n") + 1
	end := start
	for end < len(code) && (code[end] == '\t' || code[end] == ' ') {
		end++
	}
	return code[start:end]
}

// reindent indents the lines of text after its first by base, plus one tab per bracket open at
// the line's start, as gofmt would: the rules emit code without indentation.
func reindent(text, base string) string {
	if !strings.Contains(text, "\n") {
		return text
	}
	lines := strings.Split(text, "\n")
	depth := 0
	for i, l := range lines {
		t := strings.TrimLeft(l, " \t")
		if i > 0 && t != "" {
			d := depth
			if strings.IndexAny(t[:1], "})]") == 0 {
				d--
			}
			lines[i] = base + strings.Repeat("\t", max(d, 0)) + t
		}
		depth += strings.Count(t, "{") + strings.Count(t, "(") + strings.Count(t, "[") -
			strings.Count(t, "}") - strings.Count(t, ")") - strings.Count(t, "]")
	}
	return strings.Join(lines, "\n")
}

// setupNames are the names a setup directive may declare that the rewrite renames.
var setupNames = []struct{ old, new string }{
	{"agent.Durable", "*agent.Journal"},
	{"agent.AgentStream", "agent.RunStream"},
	{"agent.AgentEvent", "agent.RunEvent"},
	{"agent.PendingApproval", "agent.ApprovalPending"},
	{"agent.ResumeHalt", "agent.OutcomeUnknown"},
	{"agent.Interrupted", "agent.InterruptPending"},
	{"agent.Awaiting", "agent.SignalPending"},
	{"agent.Sleeping", "agent.TimerPending"},
	{"agent.ScriptedModel", "agenttest.ScriptedModel"},
	{"mcp.", "mcptools."},
}

var setupDirective = regexp.MustCompile(`(?s)<!--\s*docsnip:\s*setup.*?-->`)

// rewriteSetups renames, in every setup directive of a markdown document, the names that moved.
func rewriteSetups(md string) string {
	return setupDirective.ReplaceAllStringFunc(md, func(d string) string {
		for _, n := range setupNames {
			d = regexp.MustCompile(`(^|[^\w.])`+regexp.QuoteMeta(n.old)+`\b`).ReplaceAllString(d, "${1}"+n.new)
		}
		return d
	})
}
