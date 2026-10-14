package runview

import (
	"html/template"
	"net/url"
	"strings"
)

// Head is what a page's <head> carries that differs between pages: its title and its icon.
type Head struct {
	Title string
	Icon  template.URL
	// Refresh, when positive, makes the page reload itself after that many seconds (a live page
	// that needs no script).
	Refresh int
}

// faviconColors are the verdict classes' colors, mid-tones that read on a light and a dark tab bar.
var faviconColors = map[string]string{"good": "#2f9e4f", "bad": "#d9372b", "wait": "#3a74d6", "warn": "#c98a00", "info": "#6b6b66"}

// head builds a page's Head: its title, and a tab icon colored by the verdict class (a rounded
// square holding the verdict's glyph), so a tab strip of runs shows which need a person. The icon
// is a data: URL, so the page asks for nothing.
func head(title, class, icon string) Head {
	color, ok := faviconColors[class]
	if !ok {
		color = faviconColors["info"]
	}
	glyph, ok := icons[icon]
	if !ok {
		glyph = icons["node"]
	}
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><rect width="16" height="16" rx="3.5" fill="` + color + `"/>` +
		`<g transform="translate(2 2) scale(.75)" fill="none" stroke="#fff" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">` + glyph + `</g></svg>`
	return Head{Title: title, Icon: template.URL("data:image/svg+xml," + strings.ReplaceAll(url.QueryEscape(svg), "+", "%20"))} // built from constants and a key of icons
}

// headRefresh is head for a page that reloads itself every refresh seconds, when refresh is positive.
func headRefresh(title, class, icon string, refresh int) Head {
	h := head(title, class, icon)
	h.Refresh = refresh
	return h
}
