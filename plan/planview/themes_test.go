package planview_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bide-ai/bide/plan/planview"
	"github.com/bide-ai/bide/theme"
)

const config = `{"version": 1, "flow": "orders", "nodes": [{"name": "a", "block": "a"}], "wiring": []}`

func acme(t *testing.T) *theme.Theme {
	t.Helper()
	th, _, err := theme.Parse([]byte(`{"bide_theme":1,"name":"acme-night","base":"dark","surface":{"bg":"#0e1116","accent":"#79b8ff"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func style(t *testing.T, h http.Handler) string {
	t.Helper()
	r := httptest.NewRequest("GET", "http://127.0.0.1:8484/assets/style.css", nil)
	r.Host = "127.0.0.1:8484"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("style.css: %d", w.Code)
	}
	b, _ := io.ReadAll(w.Result().Body)
	return string(b)
}

func versions() (planview.Version, planview.Version) {
	v := planview.Version{Name: "orders.json", Data: []byte(config)}
	return v, v
}

// Both handlers take the same options.
func TestThemeOptions(t *testing.T) {
	th := acme(t)
	old, newer := versions()
	for name, build := range map[string]func(...planview.Option) http.Handler{
		"Handler": func(o ...planview.Option) http.Handler {
			return planview.Handler("orders.json", []byte(config), nil, o...)
		},
		"DiffHandler": func(o ...planview.Option) http.Handler { return planview.DiffHandler(old, newer, o...) },
	} {
		t.Run(name, func(t *testing.T) {
			if css := style(t, build()); !strings.Contains(css, `--bide-themes: "light dark high-contrast";`) {
				t.Error("no options: not the shipped themes")
			}
			css := style(t, build(planview.WithThemes(th)))
			if !strings.Contains(css, `"light dark high-contrast acme-night"`) || !strings.Contains(css, `:root[data-theme="acme-night"]`) {
				t.Error("WithThemes: the theme is not offered")
			}
			css = style(t, build(planview.WithThemes(th), planview.WithDefaultTheme("acme-night")))
			if !strings.Contains(css, `--bide-default: "acme-night";`) || strings.Contains(css, "prefers-color-scheme") {
				t.Error("WithDefaultTheme: the start is not the default")
			}
			css = style(t, build(planview.WithThemes(th), planview.WithThemeLock("acme-night")))
			if !strings.Contains(css, `--bide-lock: "acme-night";`) || strings.Contains(css, `:root[data-theme="`) {
				t.Error("WithThemeLock: the page is not locked")
			}
		})
	}
}

// A set that cannot be served panics when the handler is built, naming the function.
func TestThemeOptionsPanic(t *testing.T) {
	th := acme(t)
	old, newer := versions()
	bad := map[string][]planview.Option{
		"unknown default":  {planview.WithDefaultTheme("nope")},
		"unknown lock":     {planview.WithThemeLock("nope")},
		"lock and default": {planview.WithThemeLock("dark"), planview.WithDefaultTheme("light")},
		"same name twice":  {planview.WithThemes(th, th)},
		"zero theme":       {planview.WithThemes(&theme.Theme{})},
		"nil theme":        {planview.WithThemes(nil)},
	}
	for name, opts := range bad {
		for fn, build := range map[string]func(){
			"Handler":     func() { planview.Handler("orders.json", []byte(config), nil, opts...) },
			"DiffHandler": func() { planview.DiffHandler(old, newer, opts...) },
		} {
			func() {
				defer func() {
					r := recover()
					if s, _ := r.(string); !strings.HasPrefix(s, "planview: "+fn+": ") {
						t.Errorf("%s with %s: panic %v", fn, name, r)
					}
				}()
				build()
			}()
		}
	}
}
