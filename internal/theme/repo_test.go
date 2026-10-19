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

const colourNames = "aliceblue|antiquewhite|aqua|aquamarine|azure|beige|bisque|black|blanchedalmond|blue|blueviolet|brown|burlywood|cadetblue|chartreuse|chocolate|coral|cornflowerblue|cornsilk|crimson|cyan|darkblue|darkcyan|darkgoldenrod|darkgray|darkgreen|darkgrey|darkkhaki|darkmagenta|darkolivegreen|darkorange|darkorchid|darkred|darksalmon|darkseagreen|darkslateblue|darkslategray|darkslategrey|darkturquoise|darkviolet|deeppink|deepskyblue|dimgray|dimgrey|dodgerblue|firebrick|floralwhite|forestgreen|fuchsia|gainsboro|ghostwhite|gold|goldenrod|gray|green|greenyellow|grey|honeydew|hotpink|indianred|indigo|ivory|khaki|lavender|lavenderblush|lawngreen|lemonchiffon|lightblue|lightcoral|lightcyan|lightgoldenrodyellow|lightgray|lightgreen|lightgrey|lightpink|lightsalmon|lightseagreen|lightskyblue|lightslategray|lightslategrey|lightsteelblue|lightyellow|lime|limegreen|linen|magenta|maroon|mediumaquamarine|mediumblue|mediumorchid|mediumpurple|mediumseagreen|mediumslateblue|mediumspringgreen|mediumturquoise|mediumvioletred|midnightblue|mintcream|mistyrose|moccasin|navajowhite|navy|oldlace|olive|olivedrab|orange|orangered|orchid|palegoldenrod|palegreen|paleturquoise|palevioletred|papayawhip|peachpuff|peru|pink|plum|powderblue|purple|rebeccapurple|red|rosybrown|royalblue|saddlebrown|salmon|sandybrown|seagreen|seashell|sienna|silver|skyblue|slateblue|slategray|slategrey|snow|springgreen|steelblue|tan|teal|thistle|tomato|turquoise|violet|wheat|white|whitesmoke|yellow|yellowgreen"

var (
	commentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	ruleRE    = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	hexRE     = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	funcRE    = regexp.MustCompile(`(?i)\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color|light-dark|device-cmyk)\(`)
	nameRE2   = regexp.MustCompile(`(?i)(?:^|[\s,(:=])(?:` + colourNames + `)(?:$|[\s,;)!])`)
	hex6RE    = regexp.MustCompile(`#[0-9a-fA-F]{6}(?:[0-9a-fA-F]{2})?\b`)
	attrRE    = regexp.MustCompile(`(?i)(?:style|fill|stroke|stop-color|flood-color|color)\s*[=:]\s*\\?["']?([^"';>}]*)`)
	// assignRE finds a value a script gives a style: element.style.X = "v", style["x"] = "v",
	// style.setProperty("x", "v"), setAttribute("fill", "v"), and a colour property such as
	// fillStyle or backgroundColor assigned a string.
	assignRE = regexp.MustCompile(`(?i)(?:\.style(?:\.[a-z]+|\[["'][a-z-]+["']\])\s*=\s*|setProperty\(\s*["'][a-z-]+["']\s*,\s*|setAttribute\(\s*["'](?:fill|stroke|style|color|stop-color|flood-color)["']\s*,\s*|\b(?:fillStyle|strokeStyle|\w*Color)\s*=\s*)["'` + "`" + `]([^"'` + "`" + `]*)["'` + "`" + `]`)
	idleRE   = regexp.MustCompile(`(?i)var\(\s*--idle\s*[,)]`)
	importRE = regexp.MustCompile(`(?i)@import\b|\burl\((?:\s*["']?)(?:[^#"'\s)]|\s*$)`)
	stringRE = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`)
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
		if !idleRE.MatchString(d.value) {
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

// scanMarkup returns each colour literal in a file that is not a stylesheet: a 6 or 8 digit hex
// colour anywhere, and a colour in a style, fill, stroke or colour attribute or assignment.
func scanMarkup(src string) []string {
	var found []string
	found = append(found, hex6RE.FindAllString(src, -1)...)
	for _, m := range attrRE.FindAllStringSubmatch(src, -1) {
		v := m[1]
		if hexRE.MatchString(v) || funcRE.MatchString(v) || nameRE2.MatchString(" "+v+" ") {
			found = append(found, m[0])
		}
	}
	for _, m := range assignRE.FindAllStringSubmatch(src, -1) {
		if v := m[1]; hexRE.MatchString(v) || funcRE.MatchString(v) || nameRE2.MatchString(" "+v+" ") {
			found = append(found, m[0])
		}
	}
	return found
}

// scanCSSForms returns the CSS a page must not carry beyond literals: nested style rules (which the
// declaration scan cannot read), @import, and url() that is not a fragment reference.
func scanCSSForms(css string) []string {
	var found []string
	css = stringRE.ReplaceAllString(commentRE.ReplaceAllString(css, ""), `"S"`)
	if m := importRE.FindString(css); m != "" {
		found = append(found, m)
	}
	var stack []bool // true: an at-rule block; false: a style rule
	start := 0
	for i, c := range css {
		switch c {
		case '{':
			prelude := strings.TrimSpace(css[start:i])
			if k := strings.LastIndexAny(prelude, ";}"); k >= 0 {
				prelude = strings.TrimSpace(prelude[k+1:])
			}
			at := strings.HasPrefix(prelude, "@")
			if !at && len(stack) > 0 && !stack[len(stack)-1] {
				found = append(found, "nested rule "+prelude)
			}
			stack = append(stack, at)
			start = i + 1
		case '}':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			start = i + 1
		case ';':
			start = i + 1
		}
	}
	return found
}

// pageDirs are the directories holding the pages; legacyLiterals are files still on literal colours
// until the PR that moves them (the approve page: PR 5; the editor's dialog backdrop: PR 6). The
// list only shrinks.
var (
	pageDirs       = []string{"internal/runview", "flowedit", "approvals/page", "approvals"}
	markupLiterals = map[string]string{
		"internal/runview/head.go":  "the tab icon: a white glyph on the verdict class's mid-tone, drawn in a data: URL no theme reaches",
		"internal/runview/graph.go": "the Mermaid export: text pasted into a document, not a page",
	}
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
			if ext != ".css" && ext != ".js" && ext != ".html" && ext != ".go" {
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
				for _, f := range scanCSSForms(string(src)) {
					t.Errorf("%s: %s: a page's CSS has no nested rules, @import or url()", rel, f)
				}
				for _, f := range scanIdleCSS(string(src)) {
					t.Errorf("%s: %s: --idle is for strokes, borders and glyphs, not text or aliases", rel, f)
				}
				return nil
			}
			if _, ok := markupLiterals[rel]; !ok {
				for _, f := range scanMarkup(string(src)) {
					t.Errorf("%s: colour literal %q; use a theme token", rel, f)
				}
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
	for rel, why := range markupLiterals {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("markup entry %s (%s): %v", rel, why, err)
			continue
		}
		if len(scanMarkup(string(src))) == 0 {
			t.Errorf("%s has no colour literal left: remove it from markupLiterals (%s)", rel, why)
		}
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
		{"a custom property holds a literal", `:root { --brand: #ff0000; } .a { color: var(--brand); }`, 1},
		{"a custom property holds a name", `:root { --brand: tomato; }`, 1},
		{"a custom property holds a token", `:root { --x: var(--fg); }`, 0},
		{"a rarer name", `.a { background: tomato; border: 1px solid rebeccapurple; }`, 2},
		{"light-dark", `.a { color: light-dark(var(--fg), var(--bg)); }`, 1},
		{"color()", `.a { color: color(display-p3 1 0 0); }`, 1},
		{"device-cmyk", `.a { color: device-cmyk(0 1 1 0); }`, 1},
		{"upper case", `.a { color: WHITE; background: RGB(0 0 0); }`, 2},
		{"a name with !important", `.a { color: red!important; }`, 1},
		{"a word that holds a name", `.a { font-family: whitespace-font; }`, 0},
	} {
		if got := scanLiterals(tc.css); len(got) != tc.want {
			t.Errorf("%s: %d findings %v, want %d", tc.name, len(got), got, tc.want)
		}
	}
}

func TestScanMarkup(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      int
	}{
		{"a style attribute", `<div style="color:#f00">`, 1},
		{"a style attribute with a name", `<div style="background: tomato">`, 1},
		{"an svg fill", `<rect fill="#00f"/>`, 1},
		{"an svg stroke name", `<path stroke="red"/>`, 1},
		{"a Go string", "`<svg fill=\"#0f0\" style=\"stroke:red\">`", 2},
		{"a js assignment", `el.setAttribute("fill", "#fff"); x.style = "color: rgb(1,2,3)"`, 1},
		{"a long hex anywhere", `const c = "#1b52b0";`, 1},
		{"a token in style", `<div style="color:var(--fg)">`, 0},
		{"a template value", "`<svg style=\"color:${color}\">`", 0},
		{"fill none", `<rect fill="none" stroke="currentColor"/>`, 0},
		{"an id and a fragment", `<a href="#ic-gate"><use href="#node"/></a> $('#theme')`, 0},
		{"a style assignment with a name", `el.style.background = "red"`, 1},
		{"a style assignment with hsl", `x.style.background = "hsl(1 2% 3%)"`, 1},
		{"a style assignment with a short hex", `x.style.color = "#abc"`, 1},
		{"a template literal", "x.style.background = `red`", 1},
		{"a bracket style assignment", `x.style["background-color"] = "tomato"`, 1},
		{"a setProperty", `x.style.setProperty("color", "crimson")`, 1},
		{"a setAttribute", `x.setAttribute("fill", "#abc"); y.setAttribute("stroke", "red")`, 2},
		{"a canvas style", `ctx.fillStyle = "red"; ctx.strokeStyle = "rgb(1,2,3)"`, 2},
		{"backgroundColor", `el.backgroundColor = "rgb(0,0,0)"`, 1},
		{"a style assignment of a token", `el.style.color = "var(--fg)"; el.style.width = "10px"; el.style.background = "none"`, 0},
		{"a style assignment of a variable", `el.style.color = color; el.style.fill = c`, 0},
		{"a setAttribute of a non-colour", `x.setAttribute("class", "red"); x.setAttribute("title", "white")`, 0},
		{"a hex-looking id", `document.getElementById("#add")`, 0},
	} {
		// Two patterns can report one literal, so the test asks whether anything is found.
		if got := scanMarkup(tc.src); (len(got) > 0) != (tc.want > 0) {
			t.Errorf("%s: findings %v, want some: %v", tc.name, got, tc.want > 0)
		}
	}
}

func TestScanCSSForms(t *testing.T) {
	for _, tc := range []struct {
		name, css string
		want      int
	}{
		{"@import", `@import url("https://example.com/x.css");`, 1},
		{"@import bare", `@import "x.css";`, 1},
		{"a url", `.a { background: url(x.png); }`, 1},
		{"a quoted url", `.a { background: url("x.png"); }`, 1},
		{"a fragment url", `.a { marker-end: url(#arrow); }`, 0},
		{"a nested rule after a declaration", `.d { color: var(--fg); .e { margin: 0 } }`, 1},
		{"a nested rule first", `.d { .e { margin: 0 } color: var(--fg); }`, 1},
		{"a media rule", `@media (max-width: 1px) { .a { margin: 0 } .b { margin: 1px } }`, 0},
		{"keyframes", `@keyframes f { from { opacity: 0 } to { opacity: 1 } }`, 0},
		{"a plain sheet", `.a { margin: 0 } .b::before { content: "}"; }`, 0},
		{"in a comment", `/* @import "x"; .a { .b { } } */ .c { margin: 0 }`, 0},
	} {
		if got := scanCSSForms(tc.css); len(got) != tc.want {
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
		{"a fallback", `.a { color: var(--idle, var(--fg)); }`, 1},
		{"a currentColor fallback", `.a { color: var(--idle, currentColor); }`, 1},
		{"upper case", `.a { color: VAR(--idle); }`, 1},
		{"spaced", `.a { color: var( --idle ); }`, 1},
		{"a longer name", `.a { color: var(--idle-text); }`, 0},
	} {
		if got := scanIdleCSS(tc.css); len(got) != tc.want {
			t.Errorf("%s: %d findings %v, want %d", tc.name, len(got), got, tc.want)
		}
	}
	if !scanIdleOther(`el.style.color = "var(--idle)"`) || !scanIdleOther(`getComputedStyle(x).getPropertyValue("--idle")`) || scanIdleOther(`var(--muted)`) {
		t.Error("scanIdleOther does not find --idle in script")
	}
}
