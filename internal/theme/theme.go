// Package theme is the theme layer of bide's visual pages: a theme is a small JSON file of typed
// values (colours, enums, numbers in ranges), never CSS. Parse reads and checks one, Check measures
// it against the contrast and distinctness rules, and CSS turns checked themes into the one
// generated stylesheet of custom properties the pages load. See docs/design/themes.md.
//
// A theme holds two layers of tokens. The semantic tokens (ok, info, warning, alert, gated, effect,
// idem, idle, on) have a meaning bide fixes; a theme supplies their values and is refused if the
// values fail the checks. The surface tokens (backgrounds, text, borders, accent, syntax colours,
// fonts, density, radius) a theme sets freely inside ranges.
package theme

import (
	"embed"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/bide-ai/bide/internal/strictjson"
)

// MaxSize is the largest theme file Parse reads.
const MaxSize = 16 << 10

// Version is the only value of the bide_theme key.
const Version = 1

// slot is one colour a theme holds: its path in the file and the custom properties it sets.
type slot struct {
	path string
	css  []string
}

// slots lists every colour of a theme, in the order the stylesheet writes them. The CSS names are
// the ones the pages already use (--good, --bad, --card ...), so no page changes to adopt a theme.
var slots = []slot{
	{"surface.bg", []string{"--bg"}},
	{"surface.surface", []string{"--card"}},
	{"surface.surface2", []string{"--card-2"}},
	{"surface.text", []string{"--fg"}},
	{"surface.muted", []string{"--muted"}},
	{"surface.border", []string{"--line"}},
	{"surface.border_strong", []string{"--line-strong"}},
	{"surface.accent", []string{"--accent"}},
	{"surface.accent_bg", []string{"--accent-bg"}},
	{"surface.on_accent", []string{"--on-accent"}},
	{"surface.focus", []string{"--focus"}},
	{"surface.canvas_bg", []string{"--canvas-bg"}},
	{"surface.node_fill", []string{"--node-fill"}},
	{"semantic.ok.fg", []string{"--good"}},
	{"semantic.ok.bg", []string{"--good-bg"}},
	{"semantic.info.fg", []string{"--info"}},
	{"semantic.info.bg", []string{"--info-bg"}},
	{"semantic.warning.fg", []string{"--warn"}},
	{"semantic.warning.bg", []string{"--warn-bg"}},
	{"semantic.alert.fg", []string{"--bad"}},
	{"semantic.alert.bg", []string{"--bad-bg"}},
	{"semantic.gated.fg", []string{"--gate"}},
	{"semantic.gated.bg", []string{"--gate-bg"}},
	{"semantic.effect.fg", []string{"--side"}},
	{"semantic.effect.bg", []string{"--side-bg"}},
	{"semantic.idem.fg", []string{"--idem"}},
	{"semantic.idem.bg", []string{"--idem-bg"}},
	{"semantic.idle.fg", []string{"--idle"}},
	{"semantic.on.fg", []string{"--on-sem"}},
	{"syntax.key", []string{"--j-key"}},
	{"syntax.str", []string{"--j-str"}},
	{"syntax.num", []string{"--j-num"}},
	{"syntax.bool", []string{"--j-bool"}},
	{"syntax.null", []string{"--j-null"}},
	{"syntax.punct", []string{"--j-punct"}},
}

var slotSet = func() map[string]bool {
	m := map[string]bool{}
	for _, s := range slots {
		m[s.path] = true
	}
	return m
}()

// Theme is a parsed, resolved and checked theme: every token has a value. The zero value is not a
// theme; get one from Parse or Shipped.
type Theme struct {
	name    string
	shipped bool
	c       map[string]rgb

	shadow    string
	scrim     float64
	fontUI    string
	fontMono  string
	typeScale float64
	density   string
	radius    [3]int

	report Report
}

// wire is the file. Every field is optional on the wire (a custom theme inherits from its base);
// strictjson makes the names exact, unique and known.
type wire struct {
	Version   *int                         `json:"bide_theme"`
	Name      string                       `json:"name"`
	Base      string                       `json:"base"`
	Surface   map[string]string            `json:"surface"`
	Semantic  map[string]map[string]string `json:"semantic"`
	Syntax    map[string]string            `json:"syntax"`
	Shadow    *string                      `json:"shadow"`
	Scrim     *float64                     `json:"scrim"`
	FontUI    *string                      `json:"font_ui"`
	FontMono  *string                      `json:"font_mono"`
	TypeScale *float64                     `json:"type_scale"`
	Density   *string                      `json:"density"`
	Radius    map[string]int               `json:"radius"`
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

//go:embed themes/*.json
var shippedFS embed.FS

// shippedNames are the shipped themes in the order the toggle cycles them.
var shippedNames = []string{"light", "dark", "high-contrast"}

var (
	shippedOnce sync.Once
	shippedList []*Theme
	shippedErr  error
)

func loadShipped() {
	for _, n := range shippedNames {
		data, err := shippedFS.ReadFile("themes/" + n + ".json")
		if err != nil {
			shippedErr = err
			return
		}
		t, err := parse(data, nil)
		if err != nil {
			shippedErr = fmt.Errorf("theme: shipped theme %s: %w", n, err)
			return
		}
		if t.name != n {
			shippedErr = fmt.Errorf("theme: shipped file %s.json names itself %q", n, t.name)
			return
		}
		shippedList = append(shippedList, t)
	}
}

// Shipped returns the shipped themes (light, dark, high-contrast), in that order. A shipped theme
// is checked by the same rules as a custom one, and its colour-blindness result is an error rather
// than a warning. It panics if the embedded data does not parse, which a test rules out.
func Shipped() []*Theme {
	shippedOnce.Do(loadShipped)
	if shippedErr != nil {
		panic(shippedErr.Error())
	}
	return slices.Clone(shippedList)
}

// Lookup returns the shipped theme called name.
func Lookup(name string) (*Theme, bool) {
	for _, t := range Shipped() {
		if t.name == name {
			return t, true
		}
	}
	return nil, false
}

// Parse reads a custom theme: strict JSON of at most MaxSize bytes that names a shipped base and
// overrides some of its values. Whatever is left out comes from the base, and the checks run on the
// resolved theme, so inheriting cannot dodge them. It returns the checked theme and its report
// (which carries the advisory warnings), or an error naming every problem: a format error, or the
// failed checks, each key and value quoted.
func Parse(data []byte) (*Theme, Report, error) {
	t, err := parse(data, shippedBases())
	if err != nil {
		var ce *CheckError
		if errors.As(err, &ce) {
			return nil, ce.Report, err
		}
		return nil, Report{}, err
	}
	return t, t.report, nil
}

func shippedBases() map[string]*Theme {
	m := map[string]*Theme{}
	for _, t := range Shipped() {
		m[t.name] = t
	}
	return m
}

// parse reads a theme. With bases nil it reads a shipped theme: no base, every key present. With
// bases set it reads a custom theme: a base is required and the name is not a shipped one.
func parse(data []byte, bases map[string]*Theme) (*Theme, error) {
	if len(data) > MaxSize {
		return nil, fmt.Errorf("theme file is %d bytes; the limit is %d", len(data), MaxSize)
	}
	var w wire
	if err := strictjson.Unmarshal(data, &w, nil); err != nil {
		return nil, fmt.Errorf("theme file: %w", err)
	}
	custom := bases != nil
	var errs []string
	bad := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if w.Version == nil || *w.Version != Version {
		bad("bide_theme must be %d", Version)
	}
	if !nameRE.MatchString(w.Name) {
		bad("name %q must match %s", w.Name, nameRE)
	}
	t := &Theme{name: w.Name, shipped: !custom, c: map[string]rgb{}}
	if custom {
		if slices.Contains(shippedNames, w.Name) {
			bad("name %q is a shipped theme's", w.Name)
		}
		base, ok := bases[w.Base]
		if !ok {
			bad("base %q is not a shipped theme (%s)", w.Base, strings.Join(shippedNames, ", "))
		} else {
			for k, v := range base.c {
				t.c[k] = v
			}
			t.shadow, t.scrim, t.fontUI, t.fontMono = base.shadow, base.scrim, base.fontUI, base.fontMono
			t.typeScale, t.density, t.radius = base.typeScale, base.density, base.radius
		}
	} else if w.Base != "" {
		bad("a shipped theme has no base")
	}

	setColor := func(path, val string) {
		if !slotSet[path] {
			bad("unknown key %q", path)
			return
		}
		c, err := parseColor(val)
		if err != nil {
			bad("%s: %v", path, err)
			return
		}
		t.c[path] = c
	}
	for _, k := range sortedKeys(w.Surface) {
		setColor("surface."+k, w.Surface[k])
	}
	for _, name := range sortedKeys(w.Semantic) {
		for _, k := range sortedKeys(w.Semantic[name]) {
			setColor("semantic."+name+"."+k, w.Semantic[name][k])
		}
	}
	for _, k := range sortedKeys(w.Syntax) {
		setColor("syntax."+k, w.Syntax[k])
	}

	enum := func(key string, p *string, allowed []string, dst *string) {
		if p == nil {
			return
		}
		if !slices.Contains(allowed, *p) {
			bad("%s %q must be one of %s", key, *p, strings.Join(allowed, ", "))
			return
		}
		*dst = *p
	}
	enum("shadow", w.Shadow, shadowNames, &t.shadow)
	enum("font_ui", w.FontUI, uiFonts, &t.fontUI)
	enum("font_mono", w.FontMono, monoFonts, &t.fontMono)
	enum("density", w.Density, densityNames, &t.density)
	if w.Scrim != nil {
		if !(*w.Scrim >= 0.40 && *w.Scrim <= 0.80) {
			bad("scrim %v must be from 0.40 to 0.80", *w.Scrim)
		} else {
			t.scrim = *w.Scrim
		}
	}
	if w.TypeScale != nil {
		if !(*w.TypeScale >= 0.90 && *w.TypeScale <= 1.25) {
			bad("type_scale %v must be from 0.90 to 1.25", *w.TypeScale)
		} else {
			t.typeScale = *w.TypeScale
		}
	}
	if w.Radius != nil {
		for _, k := range sortedKeys(w.Radius) {
			i := slices.Index([]string{"sm", "md", "lg"}, k)
			if i < 0 {
				bad("unknown key %q", "radius."+k)
				continue
			}
			if v := w.Radius[k]; v < 0 || v > 16 {
				bad("radius.%s %d must be from 0 to 16", k, v)
			} else {
				t.radius[i] = v
			}
		}
	}

	// A shipped theme is complete: every key present, none inherited.
	if !custom {
		for _, s := range slots {
			if _, ok := t.c[s.path]; !ok {
				bad("shipped theme lacks %s", s.path)
			}
		}
		for key, missing := range map[string]bool{
			"shadow": w.Shadow == nil, "scrim": w.Scrim == nil, "font_ui": w.FontUI == nil, "font_mono": w.FontMono == nil,
			"type_scale": w.TypeScale == nil, "density": w.Density == nil, "radius": len(w.Radius) != 3,
		} {
			if missing {
				bad("shipped theme lacks %s", key)
			}
		}
	}
	if t.radius[0] > t.radius[1] || t.radius[1] > t.radius[2] {
		bad("radius must satisfy sm <= md <= lg, got %d, %d, %d", t.radius[0], t.radius[1], t.radius[2])
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, errors.New("theme: " + strings.Join(errs, "; "))
	}

	t.report = Check(t)
	if err := t.report.Err(); err != nil {
		return nil, err
	}
	return t, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Name is the theme's name, the value of data-theme that selects it.
func (t *Theme) Name() string { return t.name }

// Shipped reports whether the theme is one of bide's own.
func (t *Theme) Shipped() bool { return t.shipped }

// Report returns the theme's check report, which for a theme that parsed holds only warnings.
func (t *Theme) Report() Report { return t.report }

// Dark reports whether the theme's ground is dark: the relative luminance of its background is
// below 0.18. It sets color-scheme, so the browser's own controls agree with the colours.
func (t *Theme) Dark() bool { return t.c["surface.bg"].luminance() < 0.18 }

// round2 rounds to two decimals, the precision of every generated number.
func round2(v float64) float64 { return math.Round(v*100) / 100 }
