package theme

// fontStacks are the font families a theme can choose by name. A theme never writes a family: the
// stacks are bide's, so a theme cannot pick a face that renders 0 and O, or 1 and l, alike on a
// page that shows digests and ids, and no page asks for a font off this server.
var fontStacks = map[string]string{
	"system-sans":  `system-ui, -apple-system, "Segoe UI", Roboto, sans-serif`,
	"system-serif": `ui-serif, Georgia, "Times New Roman", serif`,
	"humanist":     `Seravek, "Gill Sans Nova", Ubuntu, Calibri, "DejaVu Sans", sans-serif`,
	"rounded":      `ui-rounded, "Hiragino Maru Gothic ProN", Quicksand, Comfortaa, Manjari, "Arial Rounded MT", Calibri, sans-serif`,
	"system-mono":  `ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace`,
	"code":         `"Cascadia Code", "Source Code Pro", Menlo, Consolas, "DejaVu Sans Mono", monospace`,
}

// uiFonts and monoFonts say which stacks each font key accepts: font_mono takes monospace stacks
// only, so ids and principal names stay unambiguous.
var (
	uiFonts   = []string{"system-sans", "system-serif", "humanist", "rounded"}
	monoFonts = []string{"system-mono", "code"}
)

var (
	shadowNames   = []string{"none", "soft", "strong"}
	densityNames  = []string{"compact", "normal", "roomy"}
	densityFactor = map[string]float64{"compact": 0.85, "normal": 1, "roomy": 1.2}
)

// shadowValue is the CSS of a shadow level; the dark set is deeper so it still reads on a dark ground.
func shadowValue(level string, dark bool) string {
	switch level {
	case "soft":
		if dark {
			return "0 1px 2px rgb(0 0 0 / .4), 0 4px 14px rgb(0 0 0 / .3)"
		}
		return "0 1px 2px rgb(0 0 0 / .06), 0 4px 14px rgb(0 0 0 / .05)"
	case "strong":
		if dark {
			return "0 1px 3px rgb(0 0 0 / .6), 0 6px 20px rgb(0 0 0 / .5)"
		}
		return "0 1px 3px rgb(0 0 0 / .12), 0 6px 20px rgb(0 0 0 / .12)"
	}
	return "none"
}
