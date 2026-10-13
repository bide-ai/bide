package runview

import (
	"fmt"
	"html/template"
	"slices"
	"strings"
)

// icons are the viewer's glyphs, drawn on a 16 by 16 grid with a 1.6 stroke in currentColor. The
// graph carries its own copy as <symbol>s (see iconDefs), so an exported graph is self-contained;
// the page carries one sprite (sprite) for the timeline and the verdict.
var icons = map[string]string{
	"side":     `<path d="M9 1.5 3.5 9H8l-1 5.5L12.5 7H8z"/>`,
	"read":     `<path d="M1.5 8S4 3.5 8 3.5 14.5 8 14.5 8 12 12.5 8 12.5 1.5 8 1.5 8z"/><circle cx="8" cy="8" r="2"/>`,
	"idem":     `<path d="M2.5 8a5.5 5.5 0 0 1 9.5-3.8M13.5 2v3h-3M13.5 8a5.5 5.5 0 0 1-9.5 3.8M2.5 14v-3h3"/>`,
	"unknown":  `<circle cx="8" cy="8" r="6"/><path d="M6.2 6.3a1.9 1.9 0 1 1 2.8 1.7c-.7.4-1 .8-1 1.5M8 11.6v.2"/>`,
	"ok":       `<path d="m3 8.5 3.2 3.2L13 4.8"/>`,
	"x":        `<path d="M4 4l8 8M12 4l-8 8"/>`,
	"halt":     `<path d="M8 1.8 14.5 13h-13z"/><path d="M8 6.5v3.2M8 11.4v.1"/>`,
	"clock":    `<circle cx="8" cy="8" r="6"/><path d="M8 4.5V8l2.3 1.5"/>`,
	"wait":     `<path d="M4.5 2h7M4.5 14h7M5 2v2.5L8 8l-3 3.5V14M11 2v2.5L8 8l3 3.5V14"/>`,
	"idle":     `<circle cx="8" cy="8" r="5.5" stroke-dasharray="2 2.4"/>`,
	"plus":     `<path d="M8 3v10M3 8h10"/>`,
	"minus":    `<path d="M3 8h10"/>`,
	"tilde":    `<path d="M2.5 9.5c1.5-3 3-3 5.5-1.5s4 1.5 5.5-1.5"/>`,
	"model":    `<path d="M2 3h12v8H8l-3 3v-3H2z"/>`,
	"tool":     `<circle cx="8" cy="8" r="2.2"/><path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M12.6 3.4l-1.4 1.4M4.8 11.2l-1.4 1.4"/>`,
	"approval": `<path d="M8 1.5 13.5 3.5v4c0 3.2-2.3 5.6-5.5 7-3.2-1.4-5.5-3.8-5.5-7v-4z"/><path d="m5.5 8 1.8 1.8 3.5-3.4"/>`,
	"gate":     `<rect x="3" y="7" width="10" height="7" rx="1.6"/><path d="M5.2 7V5a2.8 2.8 0 0 1 5.6 0v2"/>`,
	"end":      `<path d="M3.5 14V2M3.5 2.5h8l-1.8 3 1.8 3h-8"/>`,
	"start":    `<path d="M5 3l8 5-8 5z"/>`,
	"meta":     `<path d="M4 1.5h5.5L13 5v9.5H4z"/><path d="M9.5 1.5V5H13"/>`,
	"value":    `<path d="M5.5 2.5c-1.5 0-2 .7-2 2v1.5c0 1-.6 2-1.5 2 .9 0 1.5 1 1.5 2v1.5c0 1.3.5 2 2 2M10.5 2.5c1.5 0 2 .7 2 2v1.5c0 1 .6 2 1.5 2-.9 0-1.5 1-1.5 2v1.5c0 1.3-.5 2-2 2"/>`,
	"signal":   `<path d="M1.5 8h2.5l1.5-4.5 3 9 1.5-4.5h3"/>`,
	"node":     `<rect x="2.5" y="4" width="11" height="8" rx="2"/>`,
	"link":     `<path d="M6.5 9.5a3 3 0 0 0 4.2 0l2.3-2.3a3 3 0 0 0-4.2-4.2l-.8.8M9.5 6.5a3 3 0 0 0-4.2 0L3 8.8a3 3 0 0 0 4.2 4.2l.8-.8"/>`,
	"copy":     `<rect x="5.5" y="5.5" width="8" height="8" rx="1.6"/><path d="M10.5 5.5V4A1.5 1.5 0 0 0 9 2.5H4A1.5 1.5 0 0 0 2.5 4v5A1.5 1.5 0 0 0 4 10.5h1.5"/>`,
	"share":    `<path d="M8 10V2M5 5l3-3 3 3M3 9v4.5h10V9"/>`,
	"sun":      `<circle cx="8" cy="8" r="2.8"/><path d="M8 1.5v1.6M8 12.9v1.6M1.5 8h1.6M12.9 8h1.6M3.4 3.4l1.1 1.1M11.5 11.5l1.1 1.1M12.6 3.4l-1.1 1.1M4.5 11.5l-1.1 1.1"/>`,
}

// iconAttrs style every glyph; the symbols carry them so a glyph drawn outside the page's CSS still
// shows.
const iconAttrs = `fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"`

// iconNames is icons' keys in order, so the sprite and the graph's defs are deterministic.
func iconNames() []string {
	names := make([]string, 0, len(icons))
	for n := range icons {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// iconDefs is the <symbol> set for ids prefixed prefix ("ic-" for the page, "gi-" for a graph).
func iconDefs(prefix string, only ...string) string {
	var b strings.Builder
	for _, n := range iconNames() {
		if len(only) > 0 && !slices.Contains(only, n) {
			continue
		}
		fmt.Fprintf(&b, `<symbol id="%s%s" viewBox="0 0 16 16"><g %s>%s</g></symbol>`, prefix, n, iconAttrs, icons[n])
	}
	return b.String()
}

// sprite is the page's hidden glyph set, referenced by icon.
func sprite() template.HTML {
	return template.HTML(`<svg class="sprite" width="0" height="0" aria-hidden="true" focusable="false"><defs>` + iconDefs("ic-") + `</defs></svg>`) // built from constants
}

// icon is a glyph from the page's sprite; an unknown name draws nothing.
func icon(name string) template.HTML {
	if _, ok := icons[name]; !ok {
		return ""
	}
	return template.HTML(fmt.Sprintf(`<svg class="ic ic-%s" viewBox="0 0 16 16" aria-hidden="true" focusable="false"><use href="#ic-%s"/></svg>`, name, name)) // name is a key of icons
}
