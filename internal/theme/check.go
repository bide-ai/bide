package theme

import (
	"fmt"
	"slices"
	"strings"
)

// Severity is how a failed measure counts: an Error refuses the theme, a Warning is reported.
type Severity int

const (
	// Error refuses the theme.
	Error Severity = iota
	// Warning is reported and does not refuse.
	Warning
)

// String is "error" or "warning".
func (s Severity) String() string {
	if s == Warning {
		return "warning"
	}
	return "error"
}

// Measure is one pair a check looked at: what was measured, the threshold, and whether it holds.
type Measure struct {
	Check     string // C1 ... C8, see docs/design/themes.md
	Subject   string // the pair, for example "text on bg"
	Value     float64
	Threshold float64
	Pass      bool
	Severity  Severity // what a failure counts as; meaningful when !Pass
}

// Report holds every measure of a theme.
type Report struct {
	Theme    string
	Measures []Measure
}

// Failures returns the failed measures of the given severity.
func (r Report) Failures(s Severity) []Measure {
	var out []Measure
	for _, m := range r.Measures {
		if !m.Pass && m.Severity == s {
			out = append(out, m)
		}
	}
	return out
}

// CheckError is the error of a theme that failed its checks; Report holds every measure.
type CheckError struct{ Report Report }

// Error lists every failed measure, one to a line.
func (e *CheckError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "theme %q fails %d check(s):", e.Report.Theme, len(e.Report.Failures(Error)))
	for _, m := range e.Report.Failures(Error) {
		fmt.Fprintf(&b, "\n  %s %s: %s", m.Check, m.Subject, m.describe())
	}
	return b.String()
}

// Err returns a *CheckError if any measure failed as an error, and nil otherwise.
func (r Report) Err() error {
	if len(r.Failures(Error)) == 0 {
		return nil
	}
	return &CheckError{r}
}

func (m Measure) describe() string {
	switch m.Check {
	case "C3", "C4", "C6":
		return fmt.Sprintf("distance %.1f, needs at least %.0f", m.Value, m.Threshold)
	case "C8":
		if strings.HasSuffix(m.Subject, "chroma") {
			return fmt.Sprintf("chroma %.1f, needs at least %.0f", m.Value, m.Threshold)
		}
		return fmt.Sprintf("hue %.0f degrees; alert must be warm (340 to 100) and ok must not", m.Value)
	}
	return fmt.Sprintf("contrast %.2f:1, needs at least %.1f:1", m.Value, m.Threshold)
}

// Thresholds of the checks, from docs/design/themes.md.
const (
	textContrast       = 4.5
	nonTextContrast    = 3.0
	statusDistance     = 20.0
	domainDistance     = 10.0
	colorBlindDistance = 8.0
	minChroma          = 20.0
)

// semanticNames are the semantic tokens that have a foreground and a tint.
var semanticNames = []string{"ok", "info", "warning", "alert", "gated", "effect", "idem"}

// Check measures a resolved theme against every check (C1 to C6 and C8; C7, the ranges, is enforced
// while parsing) and returns all measures, passing and failing. The colour-blindness check C6 fails
// as an error for a shipped theme and as a warning for a custom one.
func Check(t *Theme) Report {
	r := Report{Theme: t.name}
	c := func(p string) rgb { return t.c[p] }
	fg := func(n string) rgb { return c("semantic." + n + ".fg") }
	tint := func(n string) rgb { return c("semantic." + n + ".bg") }

	ratio := func(check string, th float64, fgName string, fgc rgb, bgName string, bgc rgb) {
		v := contrast(fgc, bgc)
		r.Measures = append(r.Measures, Measure{Check: check, Subject: fgName + " on " + bgName, Value: v, Threshold: th, Pass: v >= th})
	}

	// C1: text contrast.
	for _, tx := range []string{"text", "muted"} {
		for _, bg := range []string{"bg", "surface", "surface2", "canvas_bg", "node_fill", "accent_bg"} {
			ratio("C1", textContrast, tx, c("surface."+tx), bg, c("surface."+bg))
		}
		for _, n := range semanticNames {
			ratio("C1", textContrast, tx, c("surface."+tx), n+" tint", tint(n))
		}
	}
	for _, n := range semanticNames {
		for _, bg := range []string{"bg", "surface", "surface2"} {
			ratio("C1", textContrast, n, fg(n), bg, c("surface."+bg))
		}
		ratio("C1", textContrast, n, fg(n), n+" tint", tint(n))
		ratio("C1", textContrast, "on", c("semantic.on.fg"), n, fg(n))
	}
	for _, bg := range []string{"bg", "surface", "surface2", "accent_bg"} {
		ratio("C1", textContrast, "accent", c("surface.accent"), bg, c("surface."+bg))
	}
	ratio("C1", textContrast, "on_accent", c("surface.on_accent"), "accent", c("surface.accent"))
	for _, s := range []string{"key", "str", "num", "bool", "null", "punct"} {
		for _, bg := range []string{"surface", "surface2"} {
			ratio("C1", textContrast, "syntax."+s, c("syntax."+s), bg, c("surface."+bg))
		}
	}

	// C2: non-text contrast.
	for _, bg := range []string{"bg", "surface"} {
		ratio("C2", nonTextContrast, "focus", c("surface.focus"), bg, c("surface."+bg))
		ratio("C2", nonTextContrast, "border_strong", c("surface.border_strong"), bg, c("surface."+bg))
	}
	for _, bg := range []string{"bg", "surface", "surface2", "canvas_bg"} {
		ratio("C2", nonTextContrast, "idle", c("semantic.idle.fg"), bg, c("surface."+bg))
	}
	ratio("C2", nonTextContrast, "info", fg("info"), "canvas_bg", c("surface.canvas_bg"))

	// C3: the status colours are distinct from each other. C6: and stay so for a reader with a
	// colour-vision deficiency.
	status := []string{"ok", "warning", "alert", "info"}
	for i, a := range status {
		for _, b := range status[i+1:] {
			d := deltaE2000(fg(a).lab(), fg(b).lab())
			r.Measures = append(r.Measures, Measure{Check: "C3", Subject: a + " and " + b, Value: d, Threshold: statusDistance, Pass: d >= statusDistance})
			sev := Warning
			if t.shipped {
				sev = Error
			}
			for v := protanopia; v <= tritanopia; v++ {
				d := deltaE2000(fg(a).simulate(v), fg(b).simulate(v))
				r.Measures = append(r.Measures, Measure{Check: "C6", Subject: a + " and " + b + " under " + v.String(), Value: d, Threshold: colorBlindDistance, Pass: d >= colorBlindDistance, Severity: sev})
			}
		}
	}

	// C4: the domain colours are distinct from every other semantic colour, idle included.
	domain := []string{"gated", "effect", "idem", "idle"}
	all := append(append([]string{}, semanticNames...), "idle")
	for i, a := range domain {
		for _, b := range all {
			if j := slices.Index(domain, b); b == a || (j >= 0 && j < i) {
				continue // each pair once
			}
			d := deltaE2000(fgOrIdle(t, a).lab(), fgOrIdle(t, b).lab())
			r.Measures = append(r.Measures, Measure{Check: "C4", Subject: a + " and " + b, Value: d, Threshold: domainDistance, Pass: d >= domainDistance})
		}
	}

	// C8: polarity. Alert is warm and ok is not, and the status hues have enough chroma to mean
	// anything. Warm is CIELAB hue 340 to 360 and 0 to 100 degrees.
	for _, n := range []string{"ok", "warning", "alert"} {
		ch, _ := fg(n).lch()
		r.Measures = append(r.Measures, Measure{Check: "C8", Subject: n + " chroma", Value: ch, Threshold: minChroma, Pass: ch >= minChroma})
	}
	_, hAlert := fg("alert").lch()
	_, hOK := fg("ok").lch()
	r.Measures = append(r.Measures,
		Measure{Check: "C8", Subject: "alert is warm", Value: hAlert, Threshold: 340, Pass: warm(hAlert)},
		Measure{Check: "C8", Subject: "ok is not warm", Value: hOK, Threshold: 340, Pass: !warm(hOK)})
	return r
}

func fgOrIdle(t *Theme, n string) rgb {
	if n == "idle" {
		return t.c["semantic.idle.fg"]
	}
	return t.c["semantic."+n+".fg"]
}

func warm(hue float64) bool { return hue >= 340 || hue <= 100 }
