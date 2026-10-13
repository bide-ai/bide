package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// CheckReleaseNotes reports every relative link or image in docs/releases/*.md under root. Each of
// those files becomes the body of a GitHub Release, where a path relative to the file does not
// resolve, so a link must be absolute (an image pinned to its release tag, such as
// https://raw.githubusercontent.com/bide-ai/bide/v0.15.0/docs/guides/img/a.png), an anchor or a
// mailto. A finding is "file:line: relative link <target> ...".
func CheckReleaseNotes(root string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(root, "docs", "releases", "*.md"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(root, f)
		inFence := false
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, target := range relativeLinks(line) {
				out = append(out, fmt.Sprintf("%s:%d: relative link %s: a release note becomes a GitHub Release body, where it does not resolve; use an absolute URL pinned to the release tag", filepath.ToSlash(rel), i+1, target))
			}
		}
	}
	return out, nil
}

var (
	codeSpan = regexp.MustCompile("`[^`]*`")
	mdLink   = regexp.MustCompile(`\]\(([^)\s]+)`)
	htmlSrc  = regexp.MustCompile(`(?i)\b(?:src|href)="([^"]+)"`)
)

// relativeLinks returns the relative targets of the Markdown links and images and the HTML src
// and href attributes in text, outside code spans and fenced code blocks.
func relativeLinks(text string) []string {
	var out []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
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
				if !absoluteTarget(m[1]) {
					out = append(out, m[1])
				}
			}
		}
	}
	return out
}

func absoluteTarget(t string) bool {
	for _, p := range []string{"https://", "http://", "#", "mailto:"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}
