package auditcli

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/bide-ai/bide/audit"
)

// The text report on a terminal (-color, internal/term): each verdict line keeps its words and
// gains a mark and a color, and the verb ends with a verdict banner. Under -json, or written to a
// file or a pipe, the report is plain.

// dimmed are the details a reader can skim: a signed tree's size, and hex digests and roots.
var dimmed = regexp.MustCompile(`signed tree of size \d+|\b[0-9a-f]{32,}\b`)

// styleLine is one report line in c's colors.
func (c *cli) styleLine(line string) string {
	st := c.st
	for _, p := range []struct {
		prefix string
		style  func(string) string
		mark   string
	}{
		{"OK:", st.BoldOK, st.Pass()},
		{"PASS:", st.BoldOK, st.Pass()},
		{"PASS  ", st.OK, st.Pass()},
		{"FAIL:", st.BoldFail, st.Cross()},
		{"FAIL  ", st.Fail, st.Cross()},
		{"ERROR:", st.BoldFail, st.Cross()},
		{"note:", st.Warn, st.Bang()},
	} {
		if rest, ok := strings.CutPrefix(line, p.prefix); ok {
			word := strings.TrimRight(p.prefix, " ")
			return p.mark + p.style(word) + p.prefix[len(word):] + c.dimDetail(rest)
		}
	}
	if strings.HasPrefix(line, "INFO  ") {
		return st.Dim(line)
	}
	if strings.HasPrefix(line, "evidence for run ") || strings.HasPrefix(line, "approval gate on call ") {
		return st.Heading(line)
	}
	return c.dimDetail(line)
}

func (c *cli) dimDetail(s string) string { return dimmed.ReplaceAllStringFunc(s, c.st.Dim) }

// write writes s to the report, each line styled when color is on.
func (c *cli) write(s string) {
	if c.st.On() {
		lines := strings.SplitAfter(s, "\n")
		for i, l := range lines {
			body, nl := strings.CutSuffix(l, "\n")
			if body != "" {
				body = c.styleLine(body)
			}
			if nl {
				body += "\n"
			}
			lines[i] = body
		}
		s = strings.Join(lines, "")
	}
	fmt.Fprint(c.out, s)
}

// banner is the verdict a verify verb ends with on a terminal: VERIFIED in green, or in red what
// the exit status means, with the signed tree the verb checked (its size and Merkle root) dimmed.
func (c *cli) banner(code int) string {
	st := c.st
	var head string
	switch code {
	case exitVerified:
		head = st.Pass() + st.BoldOK("VERIFIED")
	case exitNotVerified:
		head = st.Cross() + st.BoldFail("FAILED")
	case exitUnusable:
		head = st.Cross() + st.BoldFail("FAILED") + "  " + st.Fail("an input is unreadable or unusable")
	default:
		head = st.Cross() + st.BoldFail("NO VERDICT")
	}
	detail := fmt.Sprintf("exit %d", code)
	if c.head != nil {
		detail = fmt.Sprintf("tree size %d · merkle root %s · %s", c.head.Size, hex.EncodeToString(c.head.Root), detail)
	}
	return "\n" + head + "  " + st.Dim(detail) + "\n"
}

// checked records the signed tree head a verb checked, for the banner.
func (c *cli) checked(h audit.TreeHead) { c.head = &h }
