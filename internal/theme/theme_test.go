package theme

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// custom builds a custom theme file: a base plus the given top-level overrides.
func custom(base string, over map[string]any) []byte {
	m := map[string]any{"bide_theme": 1, "name": "acme-test", "base": base}
	for k, v := range over {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func surface(k, v string) map[string]any { return map[string]any{"surface": map[string]any{k: v}} }

func measure(r Report, check, subject string) (Measure, bool) {
	for _, m := range r.Measures {
		if m.Check == check && m.Subject == subject {
			return m, true
		}
	}
	return Measure{}, false
}

func reportOf(t *testing.T, data []byte) Report {
	t.Helper()
	_, r, err := Parse(data)
	if err == nil {
		return r
	}
	var ce *CheckError
	if !errors.As(err, &ce) {
		t.Fatalf("Parse: %v (want a theme or a CheckError)", err)
	}
	return ce.Report
}

func TestShippedThemes(t *testing.T) {
	got := Shipped()
	var names []string
	for _, th := range got {
		names = append(names, th.Name())
		if !th.Shipped() {
			t.Errorf("%s: Shipped() is false", th.Name())
		}
		// Every shipped theme passes every check, the colour-blindness check included.
		for _, m := range th.Report().Measures {
			if !m.Pass {
				t.Errorf("%s: %s %s fails: %s", th.Name(), m.Check, m.Subject, m.describe())
			}
		}
	}
	if want := "light dark high-contrast"; strings.Join(names, " ") != want {
		t.Errorf("shipped themes = %v, want %s", names, want)
	}
	for name, dark := range map[string]bool{"light": false, "dark": true, "high-contrast": false} {
		th, ok := Lookup(name)
		if !ok || th.Dark() != dark {
			t.Errorf("%s: Lookup = %v, Dark = %v, want %v", name, ok, th != nil && th.Dark(), dark)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup found a theme called nope")
	}
}

// TestHighContrastBar: the high-contrast theme holds a higher bar than the checks: 7:1 for text.
func TestHighContrastBar(t *testing.T) {
	th, _ := Lookup("high-contrast")
	for _, tx := range []string{"text", "muted"} {
		for _, bg := range []string{"bg", "surface", "surface2"} {
			if c := contrast(th.c["surface."+tx], th.c["surface."+bg]); c < 7 {
				t.Errorf("high-contrast: %s on %s is %.2f:1, want at least 7:1", tx, bg, c)
			}
		}
	}
}

func TestThresholds(t *testing.T) {
	want := map[string]float64{"C1": 4.5, "C2": 3, "C3": 20, "C4": 10, "C6": 8}
	th, _ := Lookup("light")
	seen := map[string]bool{}
	for _, m := range th.Report().Measures {
		if w, ok := want[m.Check]; ok {
			seen[m.Check] = true
			if m.Threshold != w {
				t.Errorf("%s %s: threshold %v, want %v", m.Check, m.Subject, m.Threshold, w)
			}
		}
	}
	for c := range want {
		if !seen[c] {
			t.Errorf("no %s measure in the report", c)
		}
	}
}

func TestParseValid(t *testing.T) {
	th, r, err := Parse(custom("dark", map[string]any{
		"surface":  map[string]any{"bg": "#0E1116", "accent": "#79b8ff"},
		"semantic": map[string]any{"ok": map[string]any{"fg": "#56d364"}},
		"density":  "roomy", "scrim": 0.5, "type_scale": 1.1, "radius": map[string]any{"sm": 2, "md": 4, "lg": 8},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if th.Name() != "acme-test" || th.Shipped() {
		t.Errorf("name %q shipped %v", th.Name(), th.Shipped())
	}
	if got := th.c["surface.bg"].String(); got != "#0e1116" {
		t.Errorf("bg = %s, want the normalised #0e1116", got)
	}
	if got := th.c["surface.surface"].String(); got != "#1c1c20" {
		t.Errorf("surface = %s, want the base's #1c1c20", got)
	}
	if len(r.Measures) == 0 {
		t.Error("no report")
	}
}

func TestParseRefuses(t *testing.T) {
	long := strings.Repeat(" ", MaxSize)
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"not json", []byte(`{`), "theme file"},
		{"trailing data", []byte(`{"bide_theme":1,"name":"abc","base":"dark"} {}`), "trailing"},
		{"too large", append(custom("dark", nil), long...), "limit"},
		{"unknown top-level key", custom("dark", map[string]any{"css": "body{}"}), "is not a field"},
		{"unknown surface key", custom("dark", surface("position", "#fff")), `unknown key "surface.position"`},
		{"unknown semantic key", custom("dark", map[string]any{"semantic": map[string]any{"danger": map[string]any{"fg": "#fff"}}}), "unknown key"},
		{"idle has no tint", custom("dark", map[string]any{"semantic": map[string]any{"idle": map[string]any{"bg": "#fff"}}}), "unknown key"},
		{"syntax key", custom("dark", map[string]any{"syntax": map[string]any{"keyword": "#fff"}}), "unknown key"},
		{"duplicate key", []byte(`{"bide_theme":1,"name":"abc","base":"dark","surface":{"bg":"#000000","bg":"#ffffff"}}`), "duplicate"},
		{"case variant key", []byte(`{"bide_theme":1,"name":"abc","base":"dark","surface":{"bg":"#000000","BG":"#ffffff"}}`), "unknown key"},
		{"case variant top-level key", []byte(`{"bide_theme":1,"name":"abc","base":"dark","Surface":{}}`), ""},
		{"version", []byte(`{"bide_theme":2,"name":"abc","base":"dark"}`), "bide_theme must be 1"},
		{"no version", []byte(`{"name":"abc","base":"dark"}`), "bide_theme must be 1"},
		{"name has capitals", []byte(`{"bide_theme":1,"name":"Abc","base":"dark"}`), "name"},
		{"name too short", []byte(`{"bide_theme":1,"name":"a","base":"dark"}`), "name"},
		{"name is a shipped name", []byte(`{"bide_theme":1,"name":"dark","base":"dark"}`), "shipped theme's"},
		{"name closes a string", []byte(`{"bide_theme":1,"name":"a\"]{}","base":"dark"}`), "name"},
		{"no base", []byte(`{"bide_theme":1,"name":"abc"}`), "base"},
		{"unknown base", []byte(`{"bide_theme":1,"name":"abc","base":"neon"}`), "base"},
		{"null colour", custom("dark", map[string]any{"surface": map[string]any{"bg": nil}}), "not a colour"},
		{"named colour", custom("dark", surface("bg", "red")), "not a colour"},
		{"rgb()", custom("dark", surface("bg", "rgb(0,0,0)")), "not a colour"},
		{"var()", custom("dark", surface("bg", "var(--card)")), "not a colour"},
		{"url()", custom("dark", surface("bg", "url(https://x/y.png)")), "not a colour"},
		{"alpha colour", custom("dark", surface("bg", "#000000ff")), "not a colour"},
		{"signed hex", custom("dark", surface("bg", "#-ff")), "not a colour"},
		{"system colour", custom("dark", surface("bg", "Canvas")), "not a colour"},
		{"scrim low", custom("dark", map[string]any{"scrim": 0.39}), "scrim"},
		{"scrim high", custom("dark", map[string]any{"scrim": 0.81}), "scrim"},
		{"type_scale low", custom("dark", map[string]any{"type_scale": 0.89}), "type_scale"},
		{"type_scale high", custom("dark", map[string]any{"type_scale": 1.26}), "type_scale"},
		{"density", custom("dark", map[string]any{"density": "huge"}), "density"},
		{"shadow", custom("dark", map[string]any{"shadow": "0 0 99px red"}), "shadow"},
		{"font family", custom("dark", map[string]any{"font_ui": "Comic Sans MS"}), "font_ui"},
		{"proportional mono", custom("dark", map[string]any{"font_mono": "humanist"}), "font_mono"},
		{"mono as ui", custom("dark", map[string]any{"font_ui": "system-mono"}), "font_ui"},
		{"radius too big", custom("dark", map[string]any{"radius": map[string]any{"lg": 17}}), "radius"},
		{"radius negative", custom("dark", map[string]any{"radius": map[string]any{"sm": -1}}), "radius"},
		{"radius order", custom("dark", map[string]any{"radius": map[string]any{"sm": 9, "md": 4, "lg": 8}}), "sm <= md <= lg"},
		{"radius key", custom("dark", map[string]any{"radius": map[string]any{"xl": 3}}), "unknown key"},
		{"radius fraction", []byte(`{"bide_theme":1,"name":"abc","base":"dark","radius":{"sm":2.5}}`), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th, _, err := Parse(tc.data)
			if err == nil {
				t.Fatalf("Parse succeeded (%v); want an error", th)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestInheritanceDoesNotDodge: the checks run on the resolved theme, so a file that overrides one
// colour of a good base is measured against the base's other colours.
func TestChecksFire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    []byte
		check   string
		subject string
	}{
		{"text on ground", custom("light", surface("text", "#f6f6f2")), "C1", "text on bg"},
		{"muted on a tint", custom("light", surface("muted", "#8a8a80")), "C1", "muted on surface2"},
		{"a status colour on its tint", custom("light", map[string]any{"semantic": map[string]any{"warning": map[string]any{"bg": "#8c5900"}}}), "C1", "warning on warning tint"},
		{"glyph on a disc", custom("light", map[string]any{"semantic": map[string]any{"on": map[string]any{"fg": "#9a9a9a"}}}), "C1", "on on ok"},
		{"syntax colour", custom("light", map[string]any{"syntax": map[string]any{"num": "#ccccaa"}}), "C1", "syntax.num on surface"},
		{"control edge", custom("light", surface("border_strong", "#d9d9d0")), "C2", "border_strong on surface"},
		{"focus ring", custom("dark", surface("focus", "#1c1c20")), "C2", "focus on bg"},
		{"idle on the ground", custom("light", map[string]any{"semantic": map[string]any{"idle": map[string]any{"fg": "#e8e8e0"}}}), "C2", "idle on bg"},
		{"two status colours alike", custom("light", map[string]any{"semantic": map[string]any{"warning": map[string]any{"fg": "#94201e"}}}), "C3", "warning and alert"},
		{"idempotent is the info blue", custom("light", map[string]any{"semantic": map[string]any{"idem": map[string]any{"fg": "#1d5a9c"}}}), "C4", "idem and info"},
		{"idle looks like ok", custom("light", map[string]any{"semantic": map[string]any{"idle": map[string]any{"fg": "#226d3b"}}}), "C4", "idle and ok"},
		{"alert is cool", custom("light", map[string]any{"semantic": map[string]any{"alert": map[string]any{"fg": "#1b52b0"}}}), "C8", "alert is warm"},
		{"ok is warm", custom("light", map[string]any{"semantic": map[string]any{"ok": map[string]any{"fg": "#94201c"}}}), "C8", "ok is not warm"},
		{"grey ok", custom("light", map[string]any{"semantic": map[string]any{"ok": map[string]any{"fg": "#4a504c"}}}), "C8", "ok chroma"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := reportOf(t, tc.data)
			m, ok := measure(r, tc.check, tc.subject)
			if !ok {
				t.Fatalf("no %s measure %q", tc.check, tc.subject)
			}
			if m.Pass {
				t.Errorf("%s %s passes (value %.2f, threshold %.1f); want a failure", tc.check, tc.subject, m.Value, m.Threshold)
			}
			if _, _, err := Parse(tc.data); err == nil {
				t.Error("Parse accepted the theme")
			}
		})
	}
}

// TestContrastBoundary: 4.54 passes and 4.48 fails on the measure itself, on a ground of white.
func TestContrastBoundary(t *testing.T) {
	for _, tc := range []struct {
		muted string
		pass  bool
	}{{"#767676", true}, {"#777777", false}} {
		d := map[string]any{"surface": map[string]any{"muted": tc.muted, "surface": "#ffffff"}}
		m, ok := measure(reportOf(t, custom("light", d)), "C1", "muted on surface")
		if !ok || m.Pass != tc.pass {
			t.Errorf("muted %s on white: pass = %v (%.3f), want %v", tc.muted, m.Pass, m.Value, tc.pass)
		}
	}
}

// TestColorBlindIsAWarningForCustom: a custom theme whose status colours merge for a colour-blind
// reader is accepted with a warning; the same pair in a shipped theme is an error.
func TestColorBlindSeverity(t *testing.T) {
	// The earlier ok, warning and alert of the light palette sat at one lightness.
	data := custom("light", map[string]any{"semantic": map[string]any{
		"ok": map[string]any{"fg": "#1b6a32"}, "warning": map[string]any{"fg": "#835400"}, "alert": map[string]any{"fg": "#b0241c"},
	}})
	th, r, err := Parse(data)
	if err != nil {
		t.Fatalf("a custom theme failing only C6 was refused: %v", err)
	}
	if len(r.Failures(Warning)) == 0 {
		t.Fatal("expected C6 warnings for ok, warning and alert at one lightness")
	}
	for _, m := range r.Failures(Warning) {
		if m.Check != "C6" {
			t.Errorf("a warning from %s %s", m.Check, m.Subject)
		}
	}
	if len(th.Report().Failures(Error)) != 0 {
		t.Error("errors in an accepted theme")
	}
	// The same colours, checked as a shipped theme, are an error.
	sh := *th
	sh.shipped = true
	if len(Check(&sh).Failures(Error)) == 0 {
		t.Error("C6 failures of a shipped theme are not errors")
	}
}

// TestErrorsAreQuoted: a hostile file cannot put a terminal escape into an error.
func TestErrorsAreQuoted(t *testing.T) {
	for _, data := range [][]byte{
		custom("dark", map[string]any{"surface": map[string]any{"bg": "\x1b[31mred"}}),
		custom("dark", map[string]any{"surface": map[string]any{"\x1b[2J": "#fff"}}),
		custom("dark", map[string]any{"semantic": map[string]any{"\x1b]0;x\x07": map[string]any{"fg": "#fff"}}}),
		[]byte("{\"bide_theme\":1,\"name\":\"abc\",\"base\":\"dark\",\"\x1b[31m\":1}"),
		[]byte("{\"bide_theme\":1,\"name\":\"\x1b[31m\",\"base\":\"dark\"}"),
		[]byte("{\"bide_theme\":1,\"name\":\"abc\",\"base\":\"\x1b[31m\"}"),
	} {
		_, _, err := Parse(data)
		if err == nil {
			t.Errorf("accepted %q", data)
			continue
		}
		for _, c := range err.Error() {
			if c < 0x20 && c != '\n' || c == 0x7f {
				t.Errorf("error for %q holds the control character %q: %v", data, c, err)
				break
			}
		}
	}
}

// TestShippedMustBeComplete: a shipped theme lists every key; one missing is an error, not an
// inheritance from some other theme.
func TestShippedMustBeComplete(t *testing.T) {
	data, _ := shippedFS.ReadFile("themes/dark.json")
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	delete(m["surface"].(map[string]any), "focus")
	broken, _ := json.Marshal(m)
	if _, err := parse(broken, nil); err == nil || !strings.Contains(err.Error(), "surface.focus") {
		t.Errorf("a shipped theme without surface.focus: %v", err)
	}
	m["base"] = "light"
	withBase, _ := json.Marshal(m)
	if _, err := parse(withBase, nil); err == nil {
		t.Error("a shipped theme with a base was accepted")
	}
}

// TestPolarityBoundary: warm is hue 340 to 100 degrees. A yellow-green at 103.8 is not warm, so it
// may be ok; one at 94.8 is, so it may not.
func TestPolarityBoundary(t *testing.T) {
	for _, tc := range []struct {
		ok      string
		notWarm bool
	}{{"#7a7a1f", true}, {"#8a7a1f", false}} {
		d := custom("light", map[string]any{"semantic": map[string]any{"ok": map[string]any{"fg": tc.ok}}})
		m, ok := measure(reportOf(t, d), "C8", "ok is not warm")
		if !ok || m.Pass != tc.notWarm {
			t.Errorf("ok %s: not warm = %v (hue %.1f), want %v", tc.ok, m.Pass, m.Value, tc.notWarm)
		}
	}
}

func TestChromaThreshold(t *testing.T) {
	th, _ := Lookup("light")
	for _, n := range []string{"ok", "warning", "alert"} {
		m, ok := measure(th.Report(), "C8", n+" chroma")
		if !ok || m.Threshold != 20 {
			t.Errorf("%s chroma measure = %+v, %v; want threshold 20", n, m, ok)
		}
	}
}

// TestAccentTint: muted text on the accent tint is measured (the current row, the selected item).
func TestAccentTint(t *testing.T) {
	m, ok := measure(reportOf(t, custom("light", surface("accent_bg", "#55554e"))), "C1", "muted on accent_bg")
	if !ok || m.Pass {
		t.Errorf("muted on a dark accent_bg: %+v, %v; want a failure", m, ok)
	}
}

// TestDarkBoundary: color-scheme follows the luminance of the background (dark below 0.18).
func TestDarkBoundary(t *testing.T) {
	for hex, dark := range map[string]bool{"#747474": true, "#777777": false, "#121214": true, "#ffffff": false} {
		th := &Theme{c: map[string]rgb{"surface.bg": mustColor(t, hex)}}
		if th.Dark() != dark {
			t.Errorf("bg %s: Dark = %v, want %v (luminance %.3f)", hex, th.Dark(), dark, th.c["surface.bg"].luminance())
		}
	}
}

// TestWarmLowerBoundary: the warm range starts at hue 340.
func TestWarmLowerBoundary(t *testing.T) {
	for _, tc := range []struct {
		alert string
		warm  bool
	}{{"#a0207a", true}, {"#a02080", false}} {
		d := custom("light", map[string]any{"semantic": map[string]any{"alert": map[string]any{"fg": tc.alert}}})
		m, ok := measure(reportOf(t, d), "C8", "alert is warm")
		if !ok || m.Pass != tc.warm {
			t.Errorf("alert %s: warm = %v (hue %.1f), want %v", tc.alert, m.Pass, m.Value, tc.warm)
		}
	}
}

// TestAlertWarmerThanWarning: alert and warning cannot swap warmth, even when both are warm.
func TestAlertWarmerThanWarning(t *testing.T) {
	sem := func(alert, warning string) []byte {
		return custom("light", map[string]any{"semantic": map[string]any{"alert": map[string]any{"fg": alert}, "warning": map[string]any{"fg": warning}}})
	}
	// Alert at hue 94.8 (yellow) with a warning near orange (hue 40): swapped.
	if m, ok := measure(reportOf(t, sem("#8a7a1f", "#8a4a3a")), "C8", "alert is warmer than warning"); !ok || m.Pass {
		t.Errorf("a yellow alert beside an orange warning: %+v, %v; want a failure", m, ok)
	}
	// Red alert beside an amber warning, the shipped order, passes.
	if m, ok := measure(reportOf(t, sem("#7a3030", "#8c5900")), "C8", "alert is warmer than warning"); !ok || !m.Pass {
		t.Errorf("a red alert beside an amber warning: %+v, %v; want a pass", m, ok)
	}
}

func TestHueDistance(t *testing.T) {
	for _, tc := range [][3]float64{{10, 350, 20}, {350, 10, 20}, {30, 30, 0}, {0, 180, 180}, {90, 270, 180}, {100, 30, 70}} {
		if got := hueDistance(tc[0], tc[1]); got != tc[2] {
			t.Errorf("hueDistance(%v, %v) = %v, want %v", tc[0], tc[1], got, tc[2])
		}
	}
}

func noControls(t *testing.T, what, s string) {
	t.Helper()
	for _, c := range s {
		if c < 0x20 && c != '\n' || c == 0x7f || c == 0xfffd {
			t.Errorf("%s holds the control character %q: %q", what, c, s)
			return
		}
	}
}

// TestTypeErrorsAreQuoted: encoding/json puts a map key into its type errors raw, so a key with an
// escape sequence and a value of the wrong type must still print safely.
func TestTypeErrorsAreQuoted(t *testing.T) {
	for _, data := range []string{
		"{\"bide_theme\":1,\"name\":\"zz\",\"base\":\"dark\",\"surface\":{\"\\u001b[31mRED\":5}}",
		"{\"bide_theme\":1,\"name\":\"zz\",\"base\":\"dark\",\"semantic\":{\"ok\":{\"\\u001b]0;x\\u0007\":5}}}",
		"{\"bide_theme\":1,\"name\":\"zz\",\"base\":\"dark\",\"radius\":{\"\\u001b[2J\":\"x\"}}",
		"{\"bide_theme\":1,\"name\":\"zz\",\"base\":\"dark\",\"syntax\":{\"a\\u009bb\":7}}",
	} {
		_, _, err := Parse([]byte(data))
		if err == nil {
			t.Errorf("accepted %s", data)
			continue
		}
		noControls(t, "error", err.Error())
		if !strings.Contains(err.Error(), `\x1b`) && !strings.Contains(err.Error(), `\u009b`) {
			t.Errorf("error does not show the escaped character: %v", err)
		}
		// The command prints the same text.
		_, out, errs := run("check", writeTheme(t, []byte(data)))
		noControls(t, "stdout", out)
		noControls(t, "stderr", errs)
	}
}

func TestPrintable(t *testing.T) {
	for in, want := range map[string]string{"plain é text": "plain é text", "a\nb": "a\nb", "\x1b[31m": `\x1b[31m`, "\x00": `\x00`, "bad\xffbyte": `bad\xffbyte`, "\u009b": `\u009b`, "\u2028": `\u2028`} {
		if got := printable(in); got != want {
			t.Errorf("printable(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUncheckedThemeHasNoCSS: a theme that did not come from Parse or Shipped is not a theme.
func TestUncheckedThemeHasNoCSS(t *testing.T) {
	mustPanic := func(name string, f func()) {
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		f()
	}
	mustPanic("the zero Theme", func() { CSS([]*Theme{{}}) })
	mustPanic("a zero Theme beside good ones", func() { CSS(append(Shipped(), new(Theme))) })
	d, _ := Lookup("dark")
	mustPanic("two themes with one name", func() { CSS([]*Theme{d, d}) })
}

// TestDerivedColors: a custom theme that sets accent and not info, focus, or that sets surface and
// not canvas_bg and node_fill, gets the derived colours from what it set; one that sets them keeps
// its own.
func TestDerivedColors(t *testing.T) {
	th, _, err := Parse(custom("light", map[string]any{"surface": map[string]any{
		"surface": "#fcfcfa", "accent": "#174a9e", "accent_bg": "#e0e9f8",
	}}))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"semantic.info.fg": "#174a9e", "semantic.info.bg": "#e0e9f8", "surface.focus": "#174a9e",
		"surface.canvas_bg": "#fcfcfa", "surface.node_fill": "#fcfcfa",
	} {
		if got := th.c[k].String(); got != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
	th, _, err = Parse(custom("light", map[string]any{
		"surface":  map[string]any{"surface": "#fcfcfa", "accent": "#174a9e", "focus": "#000000", "canvas_bg": "#ffffff"},
		"semantic": map[string]any{"info": map[string]any{"fg": "#1b52b0"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"semantic.info.fg": "#1b52b0", "surface.focus": "#000000", "surface.canvas_bg": "#ffffff", "surface.node_fill": "#fcfcfa"} {
		if got := th.c[k].String(); got != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
	// A theme that sets none of the sources keeps its base's values.
	th, _, _ = Parse(custom("dark", surface("text", "#ffffff")))
	if got := th.c["surface.canvas_bg"].String(); got != "#1c1c20" {
		t.Errorf("canvas_bg = %s, want the base's", got)
	}
}
