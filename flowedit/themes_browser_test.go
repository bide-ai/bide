package flowedit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// themeState is what browser/themes.mjs measured in one mode.
type themeState struct {
	BGToken       string   `json:"bgToken"`
	Attr          string   `json:"attr"`
	Current       string   `json:"current"`
	Names         []string `json:"names"`
	Forced        bool     `json:"forced"`
	SwitchHidden  bool     `json:"switchHidden"`
	SwitchDisplay string   `json:"switchDisplay"`
	SwitchLabel   string   `json:"switchLabel"`
	Primary       struct {
		Ratio    float64 `json:"ratio"`
		Text     string  `json:"text"`
		OnAccent string  `json:"onAccent"`
		Fill     string  `json:"fill"`
		Accent   string  `json:"accent"`
	} `json:"primary"`
	Controls []struct {
		Sel   string  `json:"sel"`
		Width float64 `json:"width"`
		Ratio float64 `json:"ratio"`
	} `json:"controls"`
	SelStroke string `json:"selStroke"`
	Focus     string `json:"focus"`
	Backdrop  string `json:"backdrop"`
	Scrim     string `json:"scrim"`
}

// themesRun is what browser/themes.mjs prints: a measurement per mode, and the switch's cycle.
type themesRun struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Console []string
	Results map[string]json.RawMessage `json:"results"`
}

type cycle struct {
	Names []string `json:"names"`
	Steps []struct {
		Current string `json:"current"`
		Label   string `json:"label"`
		Token   string `json:"token"`
		Status  string `json:"status"`
	} `json:"steps"`
}

func runThemes(t *testing.T, mode string, s *Server) themesRun {
	t.Helper()
	t.Setenv("MODE", mode)
	var r themesRun
	runScript(t, "themes.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q", r.Error, r.Console)
	}
	if len(r.Console) > 0 {
		t.Errorf("console: %q", r.Console)
	}
	return r
}

// mode decodes the measurement of one mode.
func (r themesRun) mode(t *testing.T, key string) themeState {
	t.Helper()
	var m themeState
	raw, ok := r.Results[key]
	if !ok {
		t.Fatalf("no measurement for %s", key)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return m
}

// checkPainted holds in every mode but forced colors: the primary button's text is the on_accent
// token at 4.5:1 on its fill, a control's edge reaches 3:1 against what it sits on (border_strong),
// the selected node's ring is the focus token, and the dialog's backdrop is the scrim token.
func checkPainted(t *testing.T, key string, m themeState) {
	t.Helper()
	if m.Primary.Text != m.Primary.OnAccent || m.Primary.Fill != m.Primary.Accent {
		t.Errorf("%s: the primary button is %s on %s, want on_accent %s on accent %s", key, m.Primary.Text, m.Primary.Fill, m.Primary.OnAccent, m.Primary.Accent)
	}
	if m.Primary.Ratio < 4.5 {
		t.Errorf("%s: the primary button's text is %.2f:1 on its fill, want 4.5:1", key, m.Primary.Ratio)
	}
	if len(m.Controls) != 3 {
		t.Errorf("%s: %d controls measured, want 3", key, len(m.Controls))
	}
	for _, c := range m.Controls {
		if c.Width < 1 || c.Ratio < 3 {
			t.Errorf("%s: the edge of %s is %.2f:1 (%gpx), want 3:1", key, c.Sel, c.Ratio, c.Width)
		}
	}
	if m.SelStroke == "" || m.SelStroke != m.Focus {
		t.Errorf("%s: the selected node's ring is %q, want the focus token %q", key, m.SelStroke, m.Focus)
	}
	if m.Backdrop != m.Scrim || m.Backdrop == "rgba(0, 0, 0, 0)" {
		t.Errorf("%s: the dialog backdrop is %q, want the scrim token %q", key, m.Backdrop, m.Scrim)
	}
}

// TestThemesInChrome: the editor draws from the checked themes in each shipped theme and each system
// mode, and its switch steps through them. It runs with BIDE_EDIT_BROWSER=1.
func TestThemesInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	r := runThemes(t, "default", s)
	for key, want := range map[string]string{
		"light": "#f6f6f2", "dark": "#121214", "high-contrast": "#ffffff", "prefers-contrast": "#ffffff", "forced-colors": "Canvas",
	} {
		m := r.mode(t, key)
		if m.BGToken != want {
			t.Errorf("%s: --bg is %q, want %q", key, m.BGToken, want)
		}
		if key == "forced-colors" {
			if !m.Forced || !strings.Contains(m.SwitchLabel, "system's colors are in force") {
				t.Errorf("forced colors: forced %v, switch label %q", m.Forced, m.SwitchLabel)
			}
			continue
		}
		checkPainted(t, key, m)
		if m.SwitchHidden {
			t.Errorf("%s: the switch is hidden on a page with three themes", key)
		}
	}
	var c cycle
	if err := json.Unmarshal(r.Results["cycle"], &c); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Names, " ") != "light dark high-contrast" || len(c.Steps) != 4 {
		t.Fatalf("the page's themes are %q with %d steps", c.Names, len(c.Steps))
	}
	for i, want := range []string{"light", "dark", "high-contrast", "light"} {
		st := c.Steps[i]
		next := c.Names[(i+1)%3]
		if st.Current != want || st.Label != "Theme: "+want+". Switch to "+next {
			t.Errorf("step %d: theme %q, label %q; want %q and a label naming %q", i, st.Current, st.Label, want, next)
		}
		if i > 0 && st.Status != "Theme: "+want {
			t.Errorf("step %d: the live region says %q, want %q", i, st.Status, "Theme: "+want)
		}
	}
	var rem struct{ Chosen, After string }
	if err := json.Unmarshal(r.Results["remembered"], &rem); err != nil {
		t.Fatal(err)
	}
	if rem.Chosen == "" || rem.Chosen != rem.After {
		t.Errorf("the choice %q was not remembered across a reload (%q)", rem.Chosen, rem.After)
	}
}

// TestLockedThemeInChrome: a host that locks the editor to one theme (WithThemeLock) gets that theme
// whatever the system prefers or a script sets, no switch (it is hidden, and a click does nothing),
// and the reader's request for more contrast or forced colors still wins.
func TestLockedThemeInChrome(t *testing.T) {
	p := filepath.Join(t.TempDir(), "refund.yaml")
	if err := os.WriteFile(p, []byte(testFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t, p, WithThemeLock("dark"))
	if err != nil {
		t.Fatal(err)
	}
	r := runThemes(t, "locked", s)
	for key, want := range map[string]string{
		"locked": "#121214", "locked+system-dark": "#121214", "locked+set-in-page": "#121214",
		"locked+prefers-contrast": "#ffffff", "locked+prefers-contrast+set-in-page": "#ffffff", "locked+forced-colors": "Canvas",
	} {
		m := r.mode(t, key)
		if m.BGToken != want {
			t.Errorf("%s: --bg is %q, want %q", key, m.BGToken, want)
		}
		if !m.SwitchHidden || m.SwitchDisplay != "none" || strings.Join(m.Names, " ") != "dark" {
			t.Errorf("%s: the page offers %q with the switch hidden=%v displayed as %q; want dark only and no switch", key, m.Names, m.SwitchHidden, m.SwitchDisplay)
		}
		if key != "locked+forced-colors" {
			checkPainted(t, key, m)
		}
	}
	var after struct{ Attr, Token string }
	if err := json.Unmarshal(r.Results["afterClick"], &after); err != nil {
		t.Fatal(err)
	}
	if after.Attr != "" || after.Token != "#121214" {
		t.Errorf("a click on the hidden switch changed the page: data-theme %q, --bg %q", after.Attr, after.Token)
	}
}
