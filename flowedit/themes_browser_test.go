package flowedit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/theme"
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
		Sel    string  `json:"sel"`
		Width  float64 `json:"width"`
		Ratio  float64 `json:"ratio"`
		Color  string  `json:"color"`
		Strong string  `json:"strong"`
	} `json:"controls"`
	SelWidth   float64 `json:"selWidth"`
	OtherWidth float64 `json:"otherWidth"`
	Audit      struct {
		Seen int      `json:"seen"`
		Bad  []string `json:"bad"`
	} `json:"audit"`
	SelStroke   string            `json:"selStroke"`
	Focus       string            `json:"focus"`
	Scrim       string            `json:"scrim"`
	LineStrong  string            `json:"lineStrong"`
	ColorScheme map[string]string `json:"colorScheme"`
	Dim         struct {
		Before []int `json:"before"`
		After  []int `json:"after"`
		Corner struct {
			Before []int   `json:"before"`
			After  []int   `json:"after"`
			Radius float64 `json:"radius"`
		} `json:"corner"`
		Edge struct {
			Width       float64 `json:"width"`
			Color       string  `json:"color"`
			Shadow      string  `json:"shadow"`
			WantShadow  string  `json:"wantShadow"`
			OverflowY   string  `json:"overflowY"`
			EscapeClose bool    `json:"escapeCloses"`
			Tall        struct {
				OnScreen   bool `json:"onScreen"`
				Scrollable bool `json:"scrollable"`
				Moved      bool `json:"moved"`
			} `json:"tall"`
		} `json:"edge"`
	} `json:"dim"`
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
func checkPainted(t *testing.T, key string, m themeState, scheme string) {
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
		if c.Color != c.Strong {
			t.Errorf("%s: the edge of %s is %s, want the border_strong token %s", key, c.Sel, c.Color, c.Strong)
		}
		if c.Width < 1 || c.Ratio < 3 {
			t.Errorf("%s: the edge of %s is %.2f:1 (%gpx), want 3:1", key, c.Sel, c.Ratio, c.Width)
		}
	}
	if m.SelStroke == "" || m.SelStroke != m.Focus {
		t.Errorf("%s: the selected node's ring is %q, want the focus token %q", key, m.SelStroke, m.Focus)
	}
	// The heaviest state stroke in graph.css is 3.8 px (a counterexample path); a selected node, whatever
	// its state, is at least 2 px heavier, so selection has a shape cue beside the colour.
	const heaviestState = 3.8
	if m.SelWidth < heaviestState+2 || m.SelWidth < m.OtherWidth+2 {
		t.Errorf("%s: the selected node's ring is %gpx (the heaviest other node %gpx, the heaviest state stroke %gpx): selection needs a shape cue beside the colour", key, m.SelWidth, m.OtherWidth, heaviestState)
	}
	if m.Audit.Seen < 12 || len(m.Audit.Bad) > 0 {
		t.Errorf("%s: %d controls audited; the theme does not reach: %q", key, m.Audit.Seen, m.Audit.Bad)
	}
	// The page tells the browser which scheme its ground is, so the native widgets (the search clear
	// button, scrollbars, checkboxes) are drawn for it.
	for sel, got := range m.ColorScheme {
		if got != scheme {
			t.Errorf("%s: color-scheme of %s is %q, want %q", key, sel, got, scheme)
		}
	}
	if len(m.ColorScheme) != 5 {
		t.Errorf("%s: color-scheme was read from %d elements, want 5", key, len(m.ColorScheme))
	}
	if m.Dim.Edge.Shadow != m.Dim.Edge.WantShadow {
		t.Errorf("%s: the dialog's elevation is %q, want the shadow token %q", key, m.Dim.Edge.Shadow, m.Dim.Edge.WantShadow)
	}
	checkDialogBehaviour(t, key, m)
	// The dialog dims the page by the scrim's alpha (a pixel far from it is the page's pixel times
	// 1 - alpha), in every engine that paints a shadow, and keeps its own edge.
	var alpha float64
	if _, err := fmt.Sscanf(m.Scrim, "rgba(0, 0, 0, %g)", &alpha); err != nil || alpha < 0.2 {
		t.Errorf("%s: the scrim token is %q, want a black with an alpha of at least 0.2", key, m.Scrim)
	}
	for i := 0; i < 3 && len(m.Dim.Before) == 3 && len(m.Dim.After) == 3; i++ {
		want := float64(m.Dim.Before[i]) * (1 - alpha)
		if got := float64(m.Dim.After[i]); got < want-3 || got > want+3 || (m.Dim.Before[i] > 40 && m.Dim.After[i] >= m.Dim.Before[i]) {
			t.Errorf("%s: channel %d of a pixel beside the dialog is %d before it opens and %d after, want about %.0f (the scrim dims the page)", key, i, m.Dim.Before[i], m.Dim.After[i], want)
		}
	}
	// The corner of the box, one pixel in and outside its rounded edge (for a radius of 12 px or more; a
	// smaller one leaves under a pixel there, which the browser antialiases), is dimmed with the rest of the page.
	if c := m.Dim.Corner; c.Radius >= 12 && len(c.Before) == 3 && len(c.After) == 3 {
		for i := 0; i < 3; i++ {
			if want := float64(c.Before[i]) * (1 - alpha); float64(c.After[i]) < want-3 || float64(c.After[i]) > want+3 {
				t.Errorf("%s: channel %d outside the dialog's rounded corner is %d, was %d, want about %.0f: the dim does not follow the corner", key, i, c.After[i], c.Before[i], want)
			}
		}
	} else if c.Radius >= 12 {
		t.Errorf("%s: the corner pixel was not read", key)
	}
	if len(m.Dim.Before) != 3 || m.Dim.Edge.Width < 1 || m.Dim.Edge.Color != m.LineStrong {
		t.Errorf("%s: the dialog's edge is %gpx %s, want 1px or more in the border_strong token %s", key, m.Dim.Edge.Width, m.Dim.Edge.Color, m.LineStrong)
	}
}

// checkDialogBehaviour: content taller than the viewport scrolls inside a box that stays on screen,
// and Escape closes the dialog.
func checkDialogBehaviour(t *testing.T, key string, m themeState) {
	t.Helper()
	tl := m.Dim.Edge.Tall
	if !tl.OnScreen || !tl.Scrollable || !tl.Moved || m.Dim.Edge.OverflowY != "auto" {
		t.Errorf("%s: a tall dialog: on screen %v, scrollable %v, scrolled %v, overflow %q", key, tl.OnScreen, tl.Scrollable, tl.Moved, m.Dim.Edge.OverflowY)
	}
	if !m.Dim.Edge.EscapeClose {
		t.Errorf("%s: Escape did not close the dialog", key)
	}
}

// checkForcedDialog: forced colors drop shadows, so the dialog does not dim the page there; it is still
// modal and keeps a visible edge in the system's border color.
func checkForcedDialog(t *testing.T, key string, m themeState) {
	t.Helper()
	checkDialogBehaviour(t, key, m)
	if m.Dim.Edge.Width < 1 || m.Dim.Edge.Color != m.LineStrong || m.Dim.Edge.Color == "rgba(0, 0, 0, 0)" {
		t.Errorf("%s: the dialog's edge is %gpx %s, want a visible edge in the system's border color %s", key, m.Dim.Edge.Width, m.Dim.Edge.Color, m.LineStrong)
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
			checkForcedDialog(t, key, m)
			continue
		}
		checkPainted(t, key, m, map[string]string{"light": "light", "dark": "dark", "high-contrast": "light", "prefers-contrast": "light"}[key])
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
	if rem.Chosen != "high-contrast" || rem.Chosen != rem.After {
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
		if key == "locked+forced-colors" {
			checkForcedDialog(t, key, m)
		} else {
			checkPainted(t, key, m, map[string]string{"locked": "dark", "locked+system-dark": "dark", "locked+set-in-page": "dark", "locked+prefers-contrast": "light", "locked+prefers-contrast+set-in-page": "light"}[key])
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

// TestCustomThemeInChrome: a theme file's own values reach the page. The theme's focus differs from
// its accent, so the selection ring and the focus outline can be told from the accent, and its
// border_strong from the hairline.
func TestCustomThemeInChrome(t *testing.T) {
	th, _, err := theme.Parse([]byte(`{"bide_theme":1,"name":"acme-focus","base":"light","surface":{"accent":"#1b52b0","focus":"#7a1fa2","border_strong":"#5a5a52"}}`))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "refund.yaml")
	if err := os.WriteFile(p, []byte(testFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t, p, WithThemes(th))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CUSTOM", "acme-focus")
	r := runThemes(t, "default", s)
	m := r.mode(t, "acme-focus")
	checkPainted(t, "acme-focus", m, "light")
	if m.Focus == m.Primary.Accent || m.SelStroke != "rgb(122, 31, 162)" {
		t.Errorf("the selected node's ring is %q (focus token %q, accent %q), want the theme's focus rgb(122, 31, 162)", m.SelStroke, m.Focus, m.Primary.Accent)
	}
	var c cycle
	if err := json.Unmarshal(r.Results["cycle"], &c); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Names, " ") != "light dark high-contrast acme-focus" {
		t.Errorf("the page's themes are %q", c.Names)
	}
}
