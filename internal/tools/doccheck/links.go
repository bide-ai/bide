package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// CheckLinks reports every relative link in the repository's documentation that does not
// resolve: a link to a file or directory that is not there, and a link to a Markdown file with a
// #fragment that names no heading (or <a id> / <a name>) in it. It reads README.md, examples/README.md
// and every Markdown file below docs, except docs/releases (their links must be absolute, see
// CheckReleaseNotes) and docs/IMPL (working plans). Links in fenced code blocks and code spans, and
// absolute URLs, are not checked. A finding is "file:line: broken link <target>: <why>".
//
// The docs site sets ignoreDeadLinks, because the docs link to source files that are not pages
// there, so this is the only check that a link in the docs goes anywhere.
func CheckLinks(root string) ([]string, error) {
	var files []string
	for _, f := range []string{"README.md", filepath.Join("examples", "README.md")} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			files = append(files, filepath.Join(root, f))
		}
	}
	docs := filepath.Join(root, "docs")
	err := filepath.WalkDir(docs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel, _ := filepath.Rel(docs, p); rel == "releases" || rel == "IMPL" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Strings(files)
	anchors := map[string]map[string]bool{}
	var out []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(root, f)
		for _, l := range markdownLinks(string(data)) {
			if why := checkLink(f, l.target, anchors); why != "" {
				out = append(out, fmt.Sprintf("%s:%d: broken link %s: %s", filepath.ToSlash(rel), l.line, l.target, why))
			}
		}
	}
	return out, nil
}

type docLink struct {
	line   int
	target string
}

// markdownLinks returns the targets of the Markdown links and images and the HTML src and href
// attributes in text, with their line numbers, outside code spans and fenced code blocks.
func markdownLinks(text string) []docLink {
	var out []docLink
	inFence := false
	for i, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		line = codeSpan.ReplaceAllString(line, "")
		for _, re := range []*regexp.Regexp{mdLink, htmlSrc} {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				out = append(out, docLink{i + 1, m[1]})
			}
		}
	}
	return out
}

// checkLink returns why target, written in file, does not resolve, or "" when it does or is not
// ours to check.
func checkLink(file, target string, anchors map[string]map[string]bool) string {
	if strings.HasPrefix(target, "<") {
		target = strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
	}
	if target == "" || strings.HasPrefix(target, "//") || regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`).MatchString(target) {
		return ""
	}
	path, frag, _ := strings.Cut(target, "#")
	path, _, _ = strings.Cut(path, "?")
	if p, err := url.PathUnescape(path); err == nil {
		path = p
	}
	if p, err := url.PathUnescape(frag); err == nil {
		frag = p
	}
	dest := file
	if path != "" {
		dest = filepath.Join(filepath.Dir(file), filepath.FromSlash(path))
		if strings.HasPrefix(path, "/") {
			return "an absolute path does not resolve on GitHub or in the docs site's source tree; link relative to this file"
		}
		if _, err := os.Stat(dest); err != nil {
			return "no such file or directory"
		}
	}
	if frag == "" || !strings.HasSuffix(dest, ".md") {
		return ""
	}
	set, ok := anchors[dest]
	if !ok {
		data, err := os.ReadFile(dest)
		if err != nil {
			return "cannot read the target to check its anchor"
		}
		set = headingAnchors(string(data))
		anchors[dest] = set
	}
	if !set[frag] {
		return "no heading or anchor #" + frag + " in " + filepath.Base(dest)
	}
	return ""
}

var (
	headingLine = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	htmlAnchor  = regexp.MustCompile(`(?i)<a\s[^>]*\b(?:id|name)="([^"]+)"`)
	inlineLink  = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// headingAnchors returns the fragments a Markdown file offers: the GitHub slug of each heading
// (repeated headings get -1, -2, ... after the first) and every id or name of an <a> tag.
func headingAnchors(text string) map[string]bool {
	set := map[string]bool{}
	seen := map[string]int{}
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, m := range htmlAnchor.FindAllStringSubmatch(line, -1) {
			set[m[1]] = true
		}
		m := headingLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		s := slug(m[1])
		if n := seen[s]; n > 0 {
			set[fmt.Sprintf("%s-%d", s, n)] = true
		} else {
			set[s] = true
		}
		seen[s]++
	}
	return set
}

// slug is GitHub's heading anchor: the text with links reduced to their labels, lower-cased,
// everything but letters, digits, spaces, hyphens and underscores dropped, and spaces made hyphens.
func slug(heading string) string {
	heading = inlineLink.ReplaceAllString(heading, "$1")
	heading = regexp.MustCompile(`</?[a-zA-Z][^>]*>`).ReplaceAllString(heading, "")
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}
