package flowedit

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/theme"
)

func acme(t *testing.T) *theme.Theme {
	t.Helper()
	th, _, err := theme.Parse([]byte(`{"bide_theme":1,"name":"acme-night","base":"dark","surface":{"bg":"#0e1116","accent":"#79b8ff"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func styleOfServer(t *testing.T, opts ...Option) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "refund.yaml")
	if err := os.WriteFile(p, []byte(testFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t, p, opts...)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://"+host+"/assets/style.css", nil)
	r.Host = host
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("style.css: %d", w.Code)
	}
	return w.Body.String()
}

func TestThemeOptions(t *testing.T) {
	th := acme(t)
	if css := styleOfServer(t); !strings.Contains(css, `--bide-themes: "light dark high-contrast";`) {
		t.Error("no options: not the shipped themes")
	}
	css := styleOfServer(t, WithThemes(th))
	if !strings.Contains(css, `"light dark high-contrast acme-night"`) || !strings.Contains(css, "--bg: #0e1116;") {
		t.Error("WithThemes: the theme is not in the editor's stylesheet")
	}
	css = styleOfServer(t, WithThemes(th), WithDefaultTheme("acme-night"))
	if !strings.Contains(css, `--bide-default: "acme-night";`) || strings.Contains(css, "prefers-color-scheme") {
		t.Error("WithDefaultTheme: the start is not the default")
	}
	css = styleOfServer(t, WithThemes(th), WithThemeLock("acme-night"))
	if !strings.Contains(css, `--bide-lock: "acme-night";`) || strings.Contains(css, `:root[data-theme="`) {
		t.Error("WithThemeLock: the page is not locked")
	}
}

// New fails, with the reason, on a set it cannot serve, and opens nothing.
func TestThemeOptionsFail(t *testing.T) {
	th := acme(t)
	p := filepath.Join(t.TempDir(), "refund.yaml")
	if err := os.WriteFile(p, []byte(testFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		opts []Option
		want string
	}{
		"unknown default":  {[]Option{WithDefaultTheme("nope")}, `default theme "nope" names no theme`},
		"unknown lock":     {[]Option{WithThemeLock("nope")}, `locked theme "nope" names no theme`},
		"lock and default": {[]Option{WithThemeLock("dark"), WithDefaultTheme("light")}, "differ"},
		"same name twice":  {[]Option{WithThemes(th, th)}, `two themes are named "acme-night"`},
		"shipped name":     {[]Option{WithThemes(theme.Shipped()[0])}, "two themes are named"},
		"zero theme":       {[]Option{WithThemes(&theme.Theme{})}, "did not come from"},
	} {
		s, err := New(p, tc.opts...)
		if err == nil {
			s.Close()
			t.Errorf("%s: New succeeded", name)
			continue
		}
		if !strings.HasPrefix(err.Error(), "flowedit: themes: ") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q, want flowedit: themes: ...%s", name, err, tc.want)
		}
	}
}
