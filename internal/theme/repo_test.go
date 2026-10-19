package theme

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The pages draw every colour through a custom property, and the themes define the properties. Two
// rules keep that true, checked here over the pages' files:
//
//   - no colour literal in a page's CSS (a hex colour, rgb(), hsl(), a colour name, or a color-mix() that mixes one),
//     since no theme could check or change it;
//   - --idle, which is held to 3:1 (C2) because it is drawn as a stroke, a border or a glyph and
//     never as text, is used only for those, and never aliased, read from script or named in a
//     template, since a 4.5:1 text colour is a different token.

var (
	commentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	ruleRE    = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	hexRE     = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	funcRE    = regexp.MustCompile(`\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(`)
	nameRE2   = regexp.MustCompile(`(?i)(?:^|[\s,(])(?:white|black|red|green|blue|gray|grey|yellow|orange|purple|pink|brown|silver|gold|navy|teal|maroon|cyan|magenta|lime|aqua|olive|fuchsia)(?:$|[\s,;)])`)
)

type decl struct{ selector, prop, value string }

// declarations lists each declaration of each rule of a stylesheet, comments removed.
func declarations(css string) []decl {
	var out []decl
	for _, m := range ruleRE.FindAllStringSubmatch(commentRE.ReplaceAllString(css, ""), -1) {
		sel := strings.TrimSpace(m[1])
		for _, d := range strings.Split(m[2], ";") {
			p, v, ok := strings.Cut(d, ":")
			if !ok {
				continue
			}
			out = append(out, decl{sel, strings.TrimSpace(p), strings.TrimSpace(v)})
		}
	}
	return out
}

// scanLiterals returns each colour literal in a stylesheet's values.
func scanLiterals(css string) []string {
	var found []string
	for _, d := range declarations(css) {
		if strings.HasPrefix(d.prop, "--") {
			continue // a custom property's value is checked where it is used
		}
		for _, re := range []*regexp.Regexp{hexRE, funcRE, nameRE2} {
			if m := re.FindString(d.value); m != "" {
				found = append(found, d.selector+" { "+d.prop+": "+d.value+" }")
				break
			}
		}
	}
	return found
}

// idleStroke are the properties that draw a line or an outline; idleFill and idleColor name the
// selectors where --idle is a shape's fill or an icon's color.
var (
	idleStroke = regexp.MustCompile(`^(?:stroke|border(?:-[a-z]+)*|outline(?:-color)?)$`)
	idleFill   = map[string]bool{".arrowhead": true}
	idleColor  = map[string]bool{".empty .ic": true}
)

// scanIdleCSS returns each use of --idle in a stylesheet that is not a stroke, a border, an
// outline, the allowed fill or the allowed icon color, and each custom property aliasing it.
func scanIdleCSS(css string) []string {
	var found []string
	for _, d := range declarations(css) {
		if !strings.Contains(d.value, "var(--idle)") {
			continue
		}
		ok := false
		switch {
		case strings.HasPrefix(d.prop, "--"):
		case idleStroke.MatchString(d.prop):
			ok = true
		case d.prop == "fill":
			ok = idleFill[d.selector]
		case d.prop == "color":
			ok = idleColor[d.selector]
		}
		if !ok {
			found = append(found, d.selector+" { "+d.prop+": "+d.value+" }")
		}
	}
	return found
}

// scanIdleOther returns whether any file that is not a stylesheet names --idle at all.
func scanIdleOther(src string) bool { return strings.Contains(src, "--idle") }

// pageDirs are the directories holding the pages; legacyLiterals are files still on literal colours
// until the PR that moves them (the approve page: PR 5; the editor's dialog backdrop: PR 6). The
// list only shrinks.
var (
	pageDirs       = []string{"internal/runview", "flowedit", "approvals/page", "approvals"}
	legacyLiterals = map[string]string{
		"approvals/page/app.css":   "the approve page moves to the themes in PR 5",
		"flowedit/page/editor.css": "dialog::backdrop's scrim moves to --scrim in PR 6",
	}
)

func TestPagesUseTokens(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	css := 0
	for _, dir := range pageDirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				if d != nil && d.IsDir() && (d.Name() == "node_modules" || d.Name() == "testdata") {
					return filepath.SkipDir
				}
				return err
			}
			rel := filepath.ToSlash(p[len(root)+1:])
			if seen[rel] || strings.HasSuffix(rel, "_test.go") {
				return nil
			}
			seen[rel] = true
			ext := filepath.Ext(p)
			if ext != ".css" && ext != ".js" && ext != ".html" && ext != ".go" && ext != ".mjs" {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if ext == ".css" {
				css++
				if _, legacy := legacyLiterals[rel]; !legacy {
					for _, f := range scanLiterals(string(src)) {
						t.Errorf("%s: colour literal %s; use a theme token", rel, f)
					}
				}
				for _, f := range scanIdleCSS(string(src)) {
					t.Errorf("%s: %s: --idle is for strokes, borders and glyphs, not text or aliases", rel, f)
				}
				return nil
			}
			if scanIdleOther(string(src)) {
				t.Errorf("%s names --idle; only a stylesheet may use it, as a stroke, border or glyph", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if css < 10 {
		t.Errorf("the scan found %d stylesheets; the page directories moved, so update the test", css)
	}
	// A legacy entry whose file is clean should be removed from the list.
	for rel, why := range legacyLiterals {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("legacy entry %s (%s): %v", rel, why, err)
			continue
		}
		if len(scanLiterals(string(src))) == 0 {
			t.Errorf("%s has no colour literal left: remove it from legacyLiterals (%s)", rel, why)
		}
	}
}

func TestScanLiterals(t *testing.T) {
	for _, tc := range []struct {
		name, css string
		want      int
	}{
		{"hex", `.a { color: #fff; }`, 1},
		{"long hex", `.a { background: #1b52b0; }`, 1},
		{"hex with alpha", `.a { fill: #1b52b0cc; }`, 1},
		{"rgb", `.a { background: rgb(0 0 0 / .45); }`, 1},
		{"rgba", `.a { background: rgba(0,0,0,.4); }`, 1},
		{"hsl", `.a { color: hsl(10 20% 30%); }`, 1},
		{"color-mix with a literal", `.a { background: color-mix(in srgb, red 10%, var(--card)); }`, 1},
		{"color-mix with a hex", `.a { background: color-mix(in srgb, #fff 10%, var(--card)); }`, 1},
		{"a name", `.a { color: white; }`, 1},
		{"a name in a shorthand", `.a { border: 1px solid black; }`, 1},
		{"in media", `@media (max-width: 1px) { .a { color: #000; } }`, 1},
		{"after a comment", `/* #fff */ .a { color: #000; }`, 1},
		{"a token", `.a { color: var(--fg); background: var(--card); }`, 0},
		{"a color-mix of tokens", `.a { background: color-mix(in srgb, var(--accent) 18%, var(--card)); box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--accent) 45%, transparent); }`, 0},
		{"a keyword", `.a { color: inherit; border-color: transparent; fill: none; stroke: currentColor; }`, 0},
		{"an id selector", `#add { color: var(--fg); } #fade { margin: 0; }`, 0},
		{"a comment", `/* white: #fff, rgb(1,2,3) */ .a { margin: 0; }`, 0},
		{"a custom property", `.a { --x: #fff; }`, 0},
		{"a word that holds a name", `.a { font-family: whitespace-font; }`, 0},
	} {
		if got := scanLiterals(tc.css); len(got) != tc.want {
			t.Errorf("%s: %d findings %v, want %d", tc.name, len(got), got, tc.want)
		}
	}
}

func TestScanIdle(t *testing.T) {
	for _, tc := range []struct {
		name, css string
		want      int
	}{
		{"a stroke", `.edge { stroke: var(--idle); }`, 0},
		{"a border", `.a { border: 2px solid var(--idle); }`, 0},
		{"a border colour", `.a { border-color: var(--idle); }`, 0},
		{"a side border", `.a { border-left-color: var(--idle); }`, 0},
		{"an outline", `.a { outline: 1px solid var(--idle); }`, 0},
		{"the arrowhead fill", `.arrowhead { fill: var(--idle); }`, 0},
		{"the icon colour", `.empty .ic { color: var(--idle); }`, 0},
		{"text colour", `.label { color: var(--idle); }`, 1},
		{"text fill", `.nlabel { fill: var(--idle); }`, 1},
		{"a background", `.a { background: var(--idle); }`, 1},
		{"a text decoration", `.a { text-decoration-color: var(--idle); }`, 1},
		{"an alias", `:root { --x: var(--idle); }`, 1},
		{"an alias in media", `@media (min-width: 1px) { .a { --x: var(--idle); } }`, 1},
		{"the icon rule's class on another selector", `.empty p { color: var(--idle); }`, 1},
		{"fill elsewhere", `.box { fill: var(--idle); }`, 1},
		{"after a comment", `/* .empty .ic */ .b { color: var(--idle); }`, 1},
		{"another token", `.a { color: var(--muted); }`, 0},
	} {
		if got := scanIdleCSS(tc.css); len(got) != tc.want {
			t.Errorf("%s: %d findings %v, want %d", tc.name, len(got), got, tc.want)
		}
	}
	if !scanIdleOther(`el.style.color = "var(--idle)"`) || !scanIdleOther(`getComputedStyle(x).getPropertyValue("--idle")`) || scanIdleOther(`var(--muted)`) {
		t.Error("scanIdleOther does not find --idle in script")
	}
}
