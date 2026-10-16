package flowedit

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// browserFail fails the test in CI (where BIDE_EDIT_BROWSER=1 means the job installed everything),
// and skips it anywhere else, so a missing Chrome cannot make the check pass silently in CI.
func browserFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// smoke is what browser/smoke.mjs prints: what the page drew and what the browser did.
type smoke struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Banner struct {
		Text     string `json:"text"`
		Role     string `json:"role"`
		Controls int    `json:"controls"`
	} `json:"banner"`
	BaselineLine    string   `json:"baselineLine"`
	Verdict         string   `json:"verdict"`
	Outline         []string `json:"outline"`
	GraphNodes      []string `json:"graphNodes"`
	SVGNamespace    string   `json:"svgNamespace"`
	Source          string   `json:"source"`
	Assertions      []string `json:"assertions"`
	InspectorBefore string   `json:"inspectorBefore"`
	ByPointer       string   `json:"byPointer"`
	Current         []string `json:"current"`
	Reached         string   `json:"reached"`
	ByKeyboard      string   `json:"byKeyboard"`
	ByKeyboardGraph string   `json:"byKeyboardGraph"`
	Live            string   `json:"live"`
	Console         []string `json:"console"`
	Failed          []string `json:"failed"`
	OffOrigin       []string `json:"offOrigin"`
}

// browserTools finds Chrome, node and the installed driver, or skips (fails, in CI).
func browserTools(t *testing.T) (node, dir, chrome string) {
	t.Helper()
	if os.Getenv("BIDE_EDIT_BROWSER") != "1" {
		t.Skip("set BIDE_EDIT_BROWSER=1 to drive the editor page in Chrome (the Editor (Chrome) job does)")
	}
	chrome = os.Getenv("CHROME_BIN")
	if chrome == "" {
		chrome, _ = exec.LookPath("google-chrome-stable")
	}
	if chrome == "" {
		browserFail(t, "Chrome is not installed (set CHROME_BIN, or install google-chrome-stable)")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		browserFail(t, "node is not installed")
	}
	dir, err = filepath.Abs("browser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "puppeteer-core")); err != nil {
		browserFail(t, "puppeteer-core is not installed in %s (npm ci --ignore-scripts)", dir)
	}
	return node, dir, chrome
}

// runSmoke serves s on a loopback port and drives its page in Chrome.
func runSmoke(t *testing.T, s *Server) smoke {
	t.Helper()
	node, dir, chrome := browserTools(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	cmd := exec.Command(node, "smoke.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CHROME_BIN="+chrome, "PAGE_URL="+ts.URL+"/#t="+s.Token())
	raw, err := cmd.Output()
	var out smoke
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		t.Fatalf("the script printed no result (%v): %q", err, raw)
	}
	if !out.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", out.Error, out.Console, out.Failed)
	}
	return out
}

// checkBrowser is what holds for every page: the same nodes in config order in the graph and the
// outline, the graph drawn in the SVG namespace, selection by pointer and by keyboard alone, the
// live region telling the selection, and no console message, failed request or request off the
// origin (a Content-Security-Policy violation is a console error).
func checkBrowser(t *testing.T, out smoke, source string) {
	t.Helper()
	if got := strings.Join(out.Outline, ","); got != "validate,charge,receipt" {
		t.Errorf("outline %q, want the nodes in config order", got)
	}
	if got := strings.Join(out.GraphNodes, ","); got != "validate,charge,receipt" {
		t.Errorf("graph nodes %q", got)
	}
	if out.SVGNamespace != "http://www.w3.org/2000/svg" {
		t.Errorf("the graph is in namespace %q, so the browser would not draw it", out.SVGNamespace)
	}
	if out.Source != source {
		t.Errorf("source %q", out.Source)
	}
	if !strings.Contains(out.InspectorBefore, "Select a node") {
		t.Errorf("inspector before a selection: %q", out.InspectorBefore)
	}
	if out.ByPointer != "charge" || len(out.Current) != 1 || out.Current[0] != "charge" {
		t.Errorf("pointer selection: inspector %q, current %q", out.ByPointer, out.Current)
	}
	if out.Reached == "" || out.ByKeyboard != out.Reached {
		t.Errorf("keyboard selection: reached %q, inspector %q", out.Reached, out.ByKeyboard)
	}
	if out.ByKeyboardGraph != "validate" {
		t.Errorf("keyboard selection in the graph: %q", out.ByKeyboardGraph)
	}
	if !strings.HasPrefix(out.Live, "Selected ") {
		t.Errorf("the live region says %q after a selection", out.Live)
	}
	if len(out.Console) > 0 || len(out.Failed) > 0 || len(out.OffOrigin) > 0 {
		t.Errorf("the browser reported console %q, failed requests %q, requests off the origin %q", out.Console, out.Failed, out.OffOrigin)
	}
}

// TestPageInChrome drives the page in real Chrome: it draws the verdict, the graph, the outline and
// the assertions the server computed, selects a node with the pointer and with the keyboard alone,
// and makes no request off its own origin, no failed request and no console message.
//
// It runs only with BIDE_EDIT_BROWSER=1 (the "Editor (Chrome)" job, .github/workflows/editor-chrome.yml,
// sets it, with CHROME_BIN and the puppeteer-core from browser/package-lock.json installed).
func TestPageInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	out := runSmoke(t, s)
	if out.Verdict != "ALL ASSERTIONS HOLD" {
		t.Errorf("verdict %q", out.Verdict)
	}
	if len(out.Assertions) != 2 || !strings.Contains(out.Assertions[0], "holds") {
		t.Errorf("assertions %q", out.Assertions)
	}
	if out.Banner.Text != "" || out.Banner.Role != "" {
		t.Errorf("a banner on a file that is not weaker than its baseline: %+v", out.Banner)
	}
	if !strings.Contains(out.BaselineLine, "not weaker than the file as it was when the editor opened it") {
		t.Errorf("baseline line %q", out.BaselineLine)
	}
	checkBrowser(t, out, testFlow)
}

// A file weaker than the committed one shows the banner the moment the page opens: with the
// classifier's words and items, announced as an alert, and with nothing in it that closes it.
func TestWeakenedBannerInChrome(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	p := filepath.Join(dir, "refund.yaml")
	weaker := strings.Replace(strings.Replace(weakFlow, "need: 2", "need: 1", 1), ">= 2 of 3", ">= 1 of 3", 1)
	write := func(text string) {
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(weakFlow)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "policy")
	write(weaker)
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	out := runSmoke(t, s)
	for _, want := range []string{"APPROVAL GATE WEAKENED", "charge: approval 2 of 3 excluding the requester, times out after 24h -> 1 of 3", "assertion loosened", "git HEAD"} {
		if !strings.Contains(out.Banner.Text, want) {
			t.Errorf("the banner lacks %q:\n%s", want, out.Banner.Text)
		}
	}
	if out.Banner.Role != "alert" {
		t.Errorf("the banner's role is %q, want alert", out.Banner.Role)
	}
	if out.Banner.Controls != 0 {
		t.Errorf("the banner holds %d controls: it must not be dismissible", out.Banner.Controls)
	}
	if !strings.Contains(out.BaselineLine, "compared with git HEAD") {
		t.Errorf("baseline line %q", out.BaselineLine)
	}
	checkBrowser(t, out, weaker)
}
