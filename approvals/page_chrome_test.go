package approvals_test

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/approvals"
)

// The approve page's look, in real Chrome: the shipped themes inside its pinned bundle, picked by the
// system's settings and by nothing else (docs/design/themes.md, option B). It runs only with
// BIDE_APPROVE_BROWSER=1 (the Viewer (Chrome) workflow sets it, after installing the driver from the
// lockfile in approvals/page/test/chrome); with CI also set a missing Chrome or driver fails instead of
// skipping. Locally: npm ci --ignore-scripts there, CHROME_BIN set, BIDE_APPROVE_BROWSER=1.

type pcolour = [3]float64

type pageText struct {
	Tag      string  `json:"tag"`
	ID       string  `json:"id"`
	Class    string  `json:"cls"`
	Text     string  `json:"text"`
	Color    pcolour `json:"color"`
	Alpha    float64 `json:"alpha"`
	BG       pcolour `json:"bg"`
	Disabled bool    `json:"disabled"`
}

type pageControl struct {
	Tag         string  `json:"tag"`
	ID          string  `json:"id"`
	Class       string  `json:"cls"`
	Disabled    bool    `json:"disabled"`
	BorderWidth float64 `json:"borderWidth"`
	BorderStyle string  `json:"borderStyle"`
	Border      pcolour `json:"border"`
	BorderAlpha float64 `json:"borderAlpha"`
	Fill        pcolour `json:"fill"`
	FillAlpha   float64 `json:"fillAlpha"`
	ParentBG    pcolour `json:"parentBG"`
}

type pageMode struct {
	Token   string        `json:"token"`
	FGToken string        `json:"fgToken"`
	Texts   []pageText    `json:"texts"`
	Control []pageControl `json:"controls"`
	Focus   struct {
		Style string  `json:"style"`
		Width float64 `json:"width"`
		Color pcolour `json:"color"`
		BG    pcolour `json:"bg"`
	} `json:"focus"`
}

func pageContrast(a, b pcolour) float64 {
	lum := func(c pcolour) float64 {
		var l [3]float64
		for i, v := range c {
			v /= 255
			if v <= 0.04045 {
				l[i] = v / 12.92
			} else {
				l[i] = math.Pow((v+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*l[0] + 0.7152*l[1] + 0.0722*l[2]
	}
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func TestPage_LookInChrome(t *testing.T) {
	if os.Getenv("BIDE_APPROVE_BROWSER") != "1" {
		t.Skip("set BIDE_APPROVE_BROWSER=1 to drive the approve page in Chrome (the Viewer (Chrome) workflow does)")
	}
	fail := func(format string, args ...any) {
		t.Helper()
		if os.Getenv("CI") != "" {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	chrome := os.Getenv("CHROME_BIN")
	if chrome == "" {
		chrome, _ = exec.LookPath("google-chrome-stable")
	}
	if chrome == "" {
		fail("Chrome is not installed (set CHROME_BIN)")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		fail("node is not installed")
	}
	dir, err := filepath.Abs("page/test/chrome")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "puppeteer-core")); err != nil {
		fail("puppeteer-core is not installed in %s (npm ci --ignore-scripts)", dir)
	}

	// The page served by an intake (approvals.WithPage), a call pending, every request allowed.
	r := newRig(t, approvals.WithPage(), approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0),
		approvals.WithAuth(func(*http.Request, approvals.Access) error { return nil }))
	cmd := exec.Command(node, "colours.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CHROME_BIN="+chrome, "PAGE_URL="+r.srv.URL+"/page/#run="+runID+"&call=c1")
	raw, runErr := cmd.Output()
	var out struct {
		OK      bool                `json:"ok"`
		Error   string              `json:"error"`
		Console []string            `json:"console"`
		Modes   map[string]pageMode `json:"modes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the script printed no result (%v): %q", runErr, raw)
	}
	if !out.OK {
		t.Fatalf("the script failed: %s", out.Error)
	}
	for _, c := range out.Console {
		t.Errorf("console: %s", c)
	}

	// Which block of the bundle's stylesheet won: the root's --bg is the shipped theme's (or the system's
	// Canvas under forced colors), a value Chrome does not force, so it shows the stylesheet's own order.
	wantToken := map[string]string{"light": "#f6f6f2", "dark": "#121214", "prefers-contrast": "#ffffff", "forced-colors": "Canvas"}
	for mode, want := range wantToken {
		m, ok := out.Modes[mode]
		if !ok {
			t.Errorf("no measurement under %s", mode)
			continue
		}
		t.Run(mode, func(t *testing.T) {
			if m.Token != want {
				t.Errorf("the root's --bg is %q, want %q: the wrong block of the bundle's stylesheet won", m.Token, want)
			}
			// Every drawn text is 4.5:1 against what it sits on (a disabled button's is exempt).
			seen := 0
			for _, x := range m.Texts {
				if x.Disabled {
					continue
				}
				seen++
				if c := pageContrast(x.Color, x.BG); c < 4.5 || x.Alpha < 0.99 {
					t.Errorf("%s#%s.%s %q: %.2f:1, alpha %v", x.Tag, x.ID, x.Class, x.Text, c, x.Alpha)
				}
			}
			if seen < 10 {
				t.Errorf("only %d texts measured; the call panel is not drawn", seen)
			}
			// Every control's edge, or its fill, is 3:1 against its surroundings; a disabled button is
			// told by a dashed edge (A12), not by color alone.
			byID := map[string]pageControl{}
			for _, c := range m.Control {
				byID[c.ID] = c
				edge := c.BorderWidth >= 1 && c.BorderStyle != "none" && c.BorderAlpha >= 0.5 && pageContrast(c.Border, c.ParentBG) >= 3
				fill := c.FillAlpha >= 0.5 && pageContrast(c.Fill, c.ParentBG) >= 3
				if !edge && !fill {
					t.Errorf("%s#%s has no 3:1 edge (border %vpx %s %v, fill %v alpha %v, on %v)", c.Tag, c.ID, c.BorderWidth, c.BorderStyle, c.Border, c.Fill, c.FillAlpha, c.ParentBG)
				}
			}
			for _, id := range []string{"approver", "approve", "reject"} {
				if _, ok := byID[id]; !ok {
					t.Errorf("control #%s was not measured", id)
				}
			}
			if a, rj := byID["approve"], byID["reject"]; a.Disabled && (a.BorderStyle != "dashed" || rj.BorderStyle != "dashed") {
				t.Errorf("disabled Approve and REJECT are %s and %s, want dashed", a.BorderStyle, rj.BorderStyle)
			}
			// The focus ring is drawn and 3:1.
			if f := m.Focus; f.Style == "none" || f.Width < 2 || pageContrast(f.Color, f.BG) < 3 {
				t.Errorf("the focus ring is %+v", f)
			}
		})
	}
	// Under dark and light the page's text really is the theme's (not a hard-coded colour): the root
	// custom property that colours it is the theme's.
	if out.Modes["light"].FGToken != "#1a1a17" || out.Modes["dark"].FGToken != "#ecece7" || !strings.EqualFold(out.Modes["prefers-contrast"].FGToken, "#000000") {
		t.Errorf("the themes' text colours are %q, %q, %q", out.Modes["light"].FGToken, out.Modes["dark"].FGToken, out.Modes["prefers-contrast"].FGToken)
	}
}
