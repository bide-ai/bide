package snip

import (
	"fmt"
	"strings"
)

// Block is one fenced ```go code block of a markdown file.
type Block struct {
	File   string // path as given to Extract, used in every report
	Line   int    // line of the block's first code line (1-based)
	Code   string // the block's content, with the fence's indentation removed
	Dir    *Directive
	Lines  int    // how many lines the content has
	Indent string // the fence's indentation, which each content line had removed
}

// Directive is the docsnip annotation directly above a block: an HTML comment
// "<!-- docsnip: skip reason -->", "<!-- docsnip: setup items -->" or
// "<!-- docsnip: api package -->".
type Directive struct {
	Line    int    // line where the comment starts
	EndLine int    // line where the comment ends
	Kind    string // "skip", "setup" or "api"
	Arg     string // the skip reason, the setup items, or the api package
}

// Extract returns every ```go block of a markdown document, each with the docsnip directive
// directly above it (blank lines may separate them). It fails on a malformed or unknown
// directive, a directive that is not followed by a go block, and an unclosed fence.
func Extract(file string, data []byte) ([]Block, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var (
		blocks  []Block
		pending *Directive
	)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<!--") && strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(trimmed, "<!--")), "docsnip:") {
			if pending != nil {
				return nil, fmt.Errorf("%s:%d: docsnip directive is not followed by a go block", file, pending.Line)
			}
			start := i
			text := trimmed
			for !strings.Contains(text, "-->") {
				i++
				if i == len(lines) {
					return nil, fmt.Errorf("%s:%d: unterminated docsnip comment", file, start+1)
				}
				text += "\n" + lines[i]
			}
			end := strings.Index(text, "-->")
			if rest := strings.TrimSpace(text[end+3:]); rest != "" {
				return nil, fmt.Errorf("%s:%d: text after a docsnip comment on the same line", file, i+1)
			}
			d, err := parseDirective(text[len("<!--"):end])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %v", file, start+1, err)
			}
			d.Line, d.EndLine = start+1, i+1
			pending = d
			continue
		}
		indent, fence, info, ok := openFence(line)
		if !ok {
			if trimmed != "" && pending != nil {
				return nil, fmt.Errorf("%s:%d: docsnip directive is not followed by a go block", file, pending.Line)
			}
			continue
		}
		var body []string
		closed := false
		first := i + 1
		for i++; i < len(lines); i++ {
			if isCloseFence(lines[i], fence) {
				closed = true
				break
			}
			body = append(body, dedent(lines[i], indent))
		}
		if !closed {
			return nil, fmt.Errorf("%s:%d: unclosed code fence", file, first)
		}
		if info != "go" {
			if pending != nil {
				return nil, fmt.Errorf("%s:%d: docsnip directive is followed by a %q block, not a go block", file, pending.Line, info)
			}
			continue
		}
		prefix := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if len(prefix) > indent {
			prefix = prefix[:indent]
		}
		blocks = append(blocks, Block{File: file, Line: first + 1, Code: strings.Join(body, "\n") + "\n", Dir: pending,
			Lines: len(body), Indent: prefix})
		pending = nil
	}
	if pending != nil {
		return nil, fmt.Errorf("%s:%d: docsnip directive is not followed by a go block", file, pending.Line)
	}
	return blocks, nil
}

func parseDirective(s string) (*Directive, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimPrefix(s, "docsnip:"))
	kind, arg := s, ""
	if i := strings.IndexAny(s, " \t\n"); i >= 0 {
		kind, arg = s[:i], strings.TrimSpace(s[i:])
	}
	switch kind {
	case "skip":
		if arg == "" {
			return nil, fmt.Errorf("docsnip: skip needs a reason")
		}
	case "setup":
		if arg == "" {
			return nil, fmt.Errorf("docsnip: setup needs at least one item")
		}
	case "api":
		pkg, _, _ := strings.Cut(arg, ";")
		if pkg = strings.TrimSpace(pkg); pkg == "" || strings.ContainsAny(pkg, " \t\n") {
			return nil, fmt.Errorf("docsnip: api needs one package name or import path, optionally followed by \"; setup items\"")
		}
	default:
		return nil, fmt.Errorf("unknown docsnip directive %q (want skip, setup or api)", kind)
	}
	return &Directive{Kind: kind, Arg: arg}, nil
}

// openFence reports whether line opens a fenced code block, with its indentation, fence
// characters and the first word of its info string.
func openFence(line string) (indent int, fence, info string, ok bool) {
	rest := strings.TrimLeft(line, " \t")
	indent = len(line) - len(rest)
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(rest) && rest[n] == c {
			n++
		}
		if n >= 3 {
			tail := rest[n:]
			if c == '`' && strings.Contains(tail, "`") {
				return 0, "", "", false // an inline code span, not a fence
			}
			word, _, _ := strings.Cut(strings.TrimSpace(tail), " ")
			return indent, rest[:n], word, true
		}
	}
	return 0, "", "", false
}

func isCloseFence(line, fence string) bool {
	rest := strings.TrimSpace(line)
	if len(rest) < len(fence) || rest[0] != fence[0] {
		return false
	}
	return strings.Trim(rest, fence[:1]) == ""
}

// dedent removes up to n leading whitespace characters, as CommonMark does for the content of
// an indented fence.
func dedent(line string, n int) string {
	i := 0
	for i < n && i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[i:]
}
