package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// source is a text that may hold override lines: a commit message or a pull request description.
type source struct {
	name, text string
}

// hunk is one hunk of a zero-context diff: the old lines [oldStart, oldStart+oldLen) and the new
// lines [newStart, newStart+newLen). A zero length means the hunk has no lines on that side.
type hunk struct {
	oldStart, oldLen, newStart, newLen int
}

// fileDiff is the diff of one file; oldPath or newPath is empty when the file was added or
// deleted.
type fileDiff struct {
	oldPath, newPath string
	hunks            []hunk
}

var (
	hunkRE     = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)
	overrideRE = regexp.MustCompile(`(?m)^[ \t]*Protocol-Impact:[ \t]*(.*?)[ \t]*\r?$`)
	// The value of an override line: an optional comma-separated list of models, "none", and a
	// reason in parentheses.
	overrideValueRE = regexp.MustCompile(`^(?:([a-z0-9_-]+(?:,[a-z0-9_-]+)*)\s+)?none\s*\((.*\S.*)\)$`)
)

// checkPathRule applies the path rule to the change from the merge base of base and head to head.
func checkPathRule(root, base, head string, models map[string]*model, bodies []source, r *report) error {
	mb, err := git(root, "merge-base", base, head)
	if err != nil {
		return err
	}
	mb = strings.TrimSpace(mb)
	out, err := git(root, "diff", "--no-renames", "--no-ext-diff", "--no-color", "-U0", mb, head)
	if err != nil {
		return err
	}
	diffs := parseDiff(out)

	specChanged := map[string]bool{}
	for _, d := range diffs {
		for _, p := range []string{d.oldPath, d.newPath} {
			for name := range models {
				if strings.HasPrefix(p, "spec/tla/"+name+"/") {
					specChanged[name] = true
				}
			}
		}
	}

	touched := map[string][]string{} // model -> where its regions were touched
	for _, d := range diffs {
		if !strings.HasSuffix(d.oldPath, ".go") && !strings.HasSuffix(d.newPath, ".go") {
			continue
		}
		var oldRegions, newRegions []region
		if d.oldPath != "" {
			src, err := git(root, "show", mb+":"+d.oldPath)
			if err != nil {
				return err
			}
			oldRegions, _ = scanRegions(d.oldPath, []byte(src), models)
		}
		if d.newPath != "" {
			src, err := git(root, "show", head+":"+d.newPath)
			if err != nil {
				return err
			}
			newRegions, _ = scanRegions(d.newPath, []byte(src), models)
		}
		for _, rg := range oldRegions {
			if hit(d.hunks, rg, true) {
				touched[rg.model] = append(touched[rg.model], fmt.Sprintf("%s:%d-%d at the base (%s)", rg.file, rg.begin, rg.end, strings.Join(rg.actions, " ")))
			}
		}
		for _, rg := range newRegions {
			if hit(d.hunks, rg, false) {
				touched[rg.model] = append(touched[rg.model], fmt.Sprintf("%s:%d-%d (%s)", rg.file, rg.begin, rg.end, strings.Join(rg.actions, " ")))
			}
		}
	}

	msgs, err := git(root, "log", "--format=%H%n%B%x00", mb+".."+head)
	if err != nil {
		return err
	}
	var texts []source
	for _, m := range strings.Split(msgs, "\x00") {
		m = strings.TrimLeft(m, "\n")
		if m == "" {
			continue
		}
		sha, body, _ := strings.Cut(m, "\n")
		texts = append(texts, source{name: "commit " + short(sha), text: body})
	}
	texts = append(texts, bodies...)
	overrides := parseOverrides(texts, models, r)

	if len(touched) == 0 {
		r.infof("path rule: no marked region changed between %s and %s", short(mb), head)
	}
	for _, name := range sortedKeys(touched) {
		where := touched[name]
		r.infof("path rule: model %s: marked regions changed:", name)
		for _, w := range dedupe(where) {
			r.infof("  %s", w)
		}
		switch {
		case specChanged[name]:
			r.infof("  ok: spec/tla/%s/ changed in the same change", name)
		case len(overrides[name]) > 0:
			for _, o := range overrides[name] {
				r.warnf("model %s: marked code changed without a model change; Protocol-Impact override from %s: %s", name, o.from, o.reason)
			}
		default:
			r.errorf("model %s: marked code changed (%s) but nothing under spec/tla/%s/ did. Update the model, or, for a change that leaves the modelled behavior as it is, add \"Protocol-Impact: none (<reason>)\" to the pull request description or a commit message.", name, strings.Join(dedupe(where), "; "), name)
		}
	}
	return nil
}

// hit reports whether a hunk touches the region: an old line of the hunk inside the region (for
// a region of the base) or a new line inside it (for a region of the head).
func hit(hunks []hunk, rg region, old bool) bool {
	for _, h := range hunks {
		start, n := h.newStart, h.newLen
		if old {
			start, n = h.oldStart, h.oldLen
		}
		if n == 0 {
			continue
		}
		if start <= rg.end && start+n-1 >= rg.begin {
			return true
		}
	}
	return false
}

// parseDiff parses the output of git diff -U0 --no-renames.
func parseDiff(out string) []fileDiff {
	var diffs []fileDiff
	var cur *fileDiff
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			diffs = append(diffs, fileDiff{})
			cur = &diffs[len(diffs)-1]
		case cur == nil:
		case strings.HasPrefix(line, "--- "):
			cur.oldPath = diffPath(line[4:], "a/")
		case strings.HasPrefix(line, "+++ "):
			cur.newPath = diffPath(line[4:], "b/")
		case strings.HasPrefix(line, "@@ "):
			m := hunkRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			cur.hunks = append(cur.hunks, hunk{atoi(m[1]), count(m[2]), atoi(m[3]), count(m[4])})
		}
	}
	// A file whose diff has no ---/+++ lines (a mode change, a binary file) changes no Go lines.
	out2 := diffs[:0]
	for _, d := range diffs {
		if d.oldPath != "" || d.newPath != "" {
			out2 = append(out2, d)
		}
	}
	return out2
}

func diffPath(p, prefix string) string {
	p = strings.TrimSuffix(p, "\t")
	if p == "/dev/null" {
		return ""
	}
	if unq, err := strconv.Unquote(p); err == nil {
		p = unq
	}
	return strings.TrimPrefix(p, prefix)
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// count is a hunk length: absent means 1.
func count(s string) int {
	if s == "" {
		return 1
	}
	return atoi(s)
}

// override is one Protocol-Impact line.
type override struct {
	from, reason string
}

// parseOverrides reads the Protocol-Impact lines of the texts, by model. A malformed line, or one
// that names a model that does not exist, is a finding.
func parseOverrides(texts []source, models map[string]*model, r *report) map[string][]override {
	out := map[string][]override{}
	for _, t := range texts {
		for _, m := range overrideRE.FindAllStringSubmatch(t.text, -1) {
			v := overrideValueRE.FindStringSubmatch(m[1])
			if v == nil {
				r.errorf("%s: malformed override %q: want \"Protocol-Impact: none (<reason>)\" or \"Protocol-Impact: <model>[,<model>] none (<reason>)\"", t.name, strings.TrimSpace(m[0]))
				continue
			}
			o := override{from: t.name, reason: strings.TrimSpace(v[2])}
			if v[1] == "" {
				for name := range models {
					out[name] = append(out[name], o)
				}
				continue
			}
			for _, name := range strings.Split(v[1], ",") {
				if models[name] == nil {
					r.errorf("%s: override names model %q, which is not a model directory under spec/tla", t.name, name)
					continue
				}
				out[name] = append(out[name], o)
			}
		}
	}
	return out
}

func git(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
