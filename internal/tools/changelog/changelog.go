package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// FragmentDir is where a pull request's changelog fragment lives, relative to the repository root.
const FragmentDir = "changes/unreleased"

// Sections are the changelog sections in the order a release lists them.
var Sections = []string{"Added", "Changed", "Fixed", "Removed", "Security"}

// Pointer is the line that stands in for the entries under "## [Unreleased]".
const Pointer = "Unreleased changes are in `changes/unreleased/`."

// Fragment is one changelog entry: the section it belongs to and its text.
type Fragment struct {
	Name    string // file name, for messages and ordering
	Section string
	Text    string
}

var sectionLine = regexp.MustCompile(`^Section: (\S+)$`)

// ParseFragment reads one fragment: a "Section: <name>" line, a blank line, then the entry text
// as one paragraph (no blank lines inside it). The text is the body of one list item.
func ParseFragment(name, content string) (Fragment, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	f := Fragment{Name: name}
	if len(lines) == 0 {
		return f, fmt.Errorf("%s: empty", name)
	}
	m := sectionLine.FindStringSubmatch(lines[0])
	if m == nil {
		return f, fmt.Errorf("%s: the first line must be \"Section: <name>\" (one of %s)", name, strings.Join(Sections, ", "))
	}
	f.Section = m[1]
	if indexOf(Sections, f.Section) < 0 {
		return f, fmt.Errorf("%s: unknown section %q (want one of %s)", name, f.Section, strings.Join(Sections, ", "))
	}
	if len(lines) < 3 || lines[1] != "" {
		return f, fmt.Errorf("%s: the \"Section:\" line must be followed by a blank line and the entry text", name)
	}
	body := strings.TrimRight(strings.Join(lines[2:], "\n"), "\n")
	if strings.TrimSpace(body) == "" {
		return f, fmt.Errorf("%s: the entry text is empty", name)
	}
	if strings.Contains(body, "\n\n") {
		return f, fmt.Errorf("%s: the entry text must be one paragraph (no blank lines)", name)
	}
	if strings.HasPrefix(body, "- ") || strings.HasPrefix(body, "#") {
		return f, fmt.Errorf("%s: write the entry text without the leading \"- \" or heading", name)
	}
	if tag := bareTag(body); tag != "" {
		return f, fmt.Errorf("%s: %q outside backticks: the docs site (VitePress) reads it as an HTML element and fails to build; put it in backticks", name, tag)
	}
	f.Text = body
	return f, nil
}

var (
	// autolink is a Markdown autolink, which renders as a link and is not an HTML element.
	autolink = regexp.MustCompile(`^<(?:[A-Za-z][A-Za-z0-9+.-]{1,31}:[^\s<>]*|[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?)>`)
	// tagStart is what makes "<" start HTML: a letter (an element), "/" (an end tag) or "!" (a
	// comment or declaration). "a < b", "<=" and "<-" are text.
	tagStart = regexp.MustCompile(`^<[A-Za-z/!]`)
)

// bareTag returns the first "<...>" in text that Markdown would pass through as HTML: one outside
// a code span, not escaped with a backslash, and not an autolink. A code span is a run of
// backticks closed by a run of the same length (CommonMark); an unclosed run is literal text.
func bareTag(text string) string {
	for i := 0; i < len(text); {
		switch {
		case text[i] == '\\' && i+1 < len(text):
			i += 2
		case text[i] == '`':
			n := 1
			for i+n < len(text) && text[i+n] == '`' {
				n++
			}
			if end := closingRun(text[i+n:], n); end >= 0 {
				i += n + end + n
			} else {
				i += n
			}
		case text[i] == '<':
			rest := text[i:]
			if autolink.MatchString(rest) || !tagStart.MatchString(rest) {
				i++
				continue
			}
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				end = len(rest) - 1
			}
			if end > 40 {
				end = 40
			}
			return rest[:end+1]
		default:
			i++
		}
	}
	return ""
}

// closingRun returns the index in s of the first run of exactly n backticks, or -1.
func closingRun(s string, n int) int {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		if j-i == n {
			return i
		}
		i = j
	}
	return -1
}

// ReadFragments parses every fragment under root/changes/unreleased, sorted by file name. A
// fragment is a .md file; other files (.gitkeep) are ignored. A missing directory holds none.
func ReadFragments(root string) ([]Fragment, error) {
	dir := filepath.Join(root, FragmentDir)
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Fragment
	var errs []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		f, err := ParseFragment(e.Name(), string(b))
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		out = append(out, f)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Section renders the body of a release's section: the "### <name>" groups in release order,
// each a list with one item per fragment, in file name order. Empty groups are left out.
func Section(frags []Fragment) string {
	var b strings.Builder
	for _, s := range Sections {
		var items []string
		for _, f := range frags {
			if f.Section == s {
				items = append(items, "- "+strings.ReplaceAll(f.Text, "\n", "\n  "))
			}
		}
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", s, strings.Join(items, "\n"))
	}
	return b.String()
}

// semver is a semantic version as scripts/release.sh reads it, without the leading v: no leading zeros,
// and an optional pre-release of non-empty dot-separated identifiers (0.21.0-rc.1).
const semver = `(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*)?`

var (
	versionRE   = regexp.MustCompile(`^` + semver + `$`)
	dateRE      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	unreleased  = regexp.MustCompile(`^\[Unreleased\]: (\S+)/compare/v(` + semver + `)\.\.\.HEAD$`)
	prRefRE     = regexp.MustCompile(`\[#(\d+)\]`)
	prDefRE     = regexp.MustCompile(`^\[#(\d+)\]: `)
	releaseHead = regexp.MustCompile(`^## \[` + semver + `\]`)
)

// Assemble returns changelog with the fragments folded in as the section for version released on
// date: the new "## [version] - date" section goes above the newest release, "## [Unreleased]"
// keeps only the pointer line, the [Unreleased] compare link moves to the new version, a
// [version] compare link is added, and a reference definition is added for every [#N] the
// entries use that has none. It fails if "## [Unreleased]" holds anything but the pointer
// (entries added to the changelog directly must be converted to fragments), if the version is
// already in the changelog, or if there are no fragments.
func Assemble(changelog string, frags []Fragment, version, date string) (string, error) {
	version = strings.TrimPrefix(version, "v")
	if !versionRE.MatchString(version) {
		return "", fmt.Errorf("version %q is not like 0.16.0 or 0.16.0-rc.1", version)
	}
	if !dateRE.MatchString(date) {
		return "", fmt.Errorf("date %q is not like 2026-10-20", date)
	}
	if len(frags) == 0 {
		return "", fmt.Errorf("no fragments in %s", FragmentDir)
	}
	lines := strings.Split(changelog, "\n")
	head, first := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "## ["+version+"]"):
			return "", fmt.Errorf("CHANGELOG.md already has a [%s] section", version)
		case l == "## [Unreleased]":
			head = i
		case head >= 0 && first < 0 && releaseHead.MatchString(l):
			first = i
		}
	}
	if head < 0 || first < 0 {
		return "", fmt.Errorf("CHANGELOG.md needs a \"## [Unreleased]\" heading followed by a released version")
	}
	for _, l := range lines[head+1 : first] {
		if t := strings.TrimSpace(l); t != "" && t != Pointer {
			return "", fmt.Errorf("\"## [Unreleased]\" holds %q: move entries to %s/ fragments", t, FragmentDir)
		}
	}
	section := Section(frags)
	out := append([]string{}, lines[:head+1]...)
	out = append(out, "", Pointer, "", "## ["+version+"] - "+date, "")
	out = append(out, strings.Split(strings.TrimRight(section, "\n"), "\n")...)
	out = append(out, "")
	out = append(out, lines[first:]...)

	// Compare links.
	link := -1
	for i, l := range out {
		if unreleased.MatchString(l) {
			link = i
			break
		}
	}
	if link < 0 {
		return "", fmt.Errorf("CHANGELOG.md has no \"[Unreleased]: .../compare/vX.Y.Z...HEAD\" link")
	}
	m := unreleased.FindStringSubmatch(out[link])
	repo, prev := m[1], m[2]
	out[link] = fmt.Sprintf("[Unreleased]: %s/compare/v%s...HEAD", repo, version)
	out = insertAt(out, link+1, fmt.Sprintf("[%s]: %s/compare/v%s...v%s", version, repo, prev, version))

	// PR reference definitions for the new entries.
	defined := map[int]bool{}
	last := -1
	for i, l := range out {
		if m := prDefRE.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			defined[n] = true
			last = i
		}
	}
	var missing []int
	for _, m := range prRefRE.FindAllStringSubmatch(section, -1) {
		n, _ := strconv.Atoi(m[1])
		if !defined[n] {
			defined[n] = true
			missing = append(missing, n)
		}
	}
	sort.Ints(missing)
	for _, n := range missing {
		def := fmt.Sprintf("[#%d]: %s/pull/%d", n, repo, n)
		if last < 0 {
			out = append(out, "", def)
			last = len(out) - 1
		} else {
			out = insertAt(out, last+1, def)
			last++
		}
	}
	return strings.Join(out, "\n"), nil
}

func insertAt(s []string, i int, v string) []string {
	s = append(s, "")
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// NeedsFragment reports whether a change to these files needs a changelog fragment: whether any
// of them is code a published module ships. Documentation, tests, CI, examples, scripts and the
// repository's own tools do not. The fragment directory itself does not either (a change that
// holds one is checked by HasFragment).
func NeedsFragment(files []string) bool {
	for _, f := range files {
		if !exempt(f) {
			return true
		}
	}
	return false
}

var exemptDirs = []string{
	"docs/", "docs-site/", "assets/", "spec/tla/", ".github/", "changes/",
	"examples/", "integration/", "benchmarks/", "scripts/", "internal/tools/",
}

func exempt(f string) bool {
	for _, d := range exemptDirs {
		if strings.HasPrefix(f, d) {
			return true
		}
	}
	if strings.HasSuffix(f, ".md") || strings.HasSuffix(f, "_test.go") {
		return true
	}
	for _, seg := range strings.Split(f, "/") {
		if seg == "testdata" {
			return true
		}
	}
	switch f {
	case "LICENSE", "NOTICE", "DCO", ".gitignore", ".gitattributes", ".doccheck-allow", "codecov.yml", "go.sum", "go.work", "go.work.sum":
		return true
	}
	return strings.HasSuffix(f, "/go.sum") || strings.HasSuffix(f, "/go.mod") || f == "go.mod"
}

// NotSafe returns the files of a change that are outside the set a no-changelog pull request may
// change: the ones NeedsFragment counts as published code. A pull request with the no-changelog
// label claims that nothing it changes needs a changelog line, and this is the claim's check.
func NotSafe(files []string) []string {
	var out []string
	for _, f := range files {
		if !exempt(f) {
			out = append(out, f)
		}
	}
	return out
}

// Stray returns the files of a change that are never meant to be committed, whatever the label:
// a Go workspace file other than go.work and go.work.sum (scratch files such as alt.work), and a
// go.mod or go.sum of a module in which the change touches no Go file (what `go work sync` leaves
// behind in every module). modules lists the directories of the repository's modules ("." for the
// root), and a file belongs to the module of its longest directory prefix.
func Stray(files, modules []string) []string {
	touched := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, ".go") {
			touched[moduleOf(f, modules)] = true
		}
	}
	var out []string
	for _, f := range files {
		base := f[strings.LastIndex(f, "/")+1:]
		switch {
		case (strings.HasSuffix(base, ".work") || strings.HasSuffix(base, ".work.sum")) && base != "go.work" && base != "go.work.sum":
			out = append(out, f)
		case (base == "go.mod" || base == "go.sum") && !touched[moduleOf(f, modules)]:
			out = append(out, f)
		}
	}
	return out
}

// moduleOf returns the directory in modules that holds file: the longest one that prefixes it.
func moduleOf(file string, modules []string) string {
	best := "."
	for _, m := range modules {
		if m != "." && strings.HasPrefix(file, m+"/") && (best == "." || len(m) > len(best)) {
			best = m
		}
	}
	return best
}

// HasFragment reports whether the added or modified files include a fragment.
func HasFragment(added []string) bool {
	for _, f := range added {
		if strings.HasPrefix(f, FragmentDir+"/") && strings.HasSuffix(f, ".md") && strings.Count(f, "/") == strings.Count(FragmentDir, "/")+1 {
			return true
		}
	}
	return false
}
