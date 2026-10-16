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

// TestPageInChrome drives the page in real Chrome: it draws the verdict, the graph, the outline and
// the assertions the server computed, selects a node with the pointer and with the keyboard alone,
// and makes no request off its own origin, no failed request and no console message (a
// Content-Security-Policy violation is a console error).
//
// It runs only with BIDE_EDIT_BROWSER=1 (the "Editor (Chrome)" job, .github/workflows/editor-chrome.yml,
// sets it, with CHROME_BIN and the puppeteer-core from browser/package-lock.json installed).
func TestPageInChrome(t *testing.T) {
	if os.Getenv("BIDE_EDIT_BROWSER") != "1" {
		t.Skip("set BIDE_EDIT_BROWSER=1 to drive the editor page in Chrome (the Editor (Chrome) job does)")
	}
	chrome := os.Getenv("CHROME_BIN")
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
	dir, err := filepath.Abs("browser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "puppeteer-core")); err != nil {
		browserFail(t, "puppeteer-core is not installed in %s (npm ci --ignore-scripts)", dir)
	}

	s, _ := newTestServer(t, "refund.yaml", testFlow)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	cmd := exec.Command(node, "smoke.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CHROME_BIN="+chrome, "PAGE_URL="+ts.URL+"/#t="+s.Token())
	raw, err := cmd.Output()
	var out struct {
		OK                bool     `json:"ok"`
		Error             string   `json:"error"`
		Verdict           string   `json:"verdict"`
		Outline           []string `json:"outline"`
		GraphNodes        []string `json:"graphNodes"`
		SVGNamespace      string   `json:"svgNamespace"`
		Source            string   `json:"source"`
		Assertions        []string `json:"assertions"`
		InspectorBefore   string   `json:"inspectorBefore"`
		ByPointer         string   `json:"byPointer"`
		Current           []string `json:"current"`
		Reached           string   `json:"reached"`
		ByKeyboard        string   `json:"byKeyboard"`
		ByKeyboardGraph   string   `json:"byKeyboardGraph"`
		Live              string   `json:"live"`
		Console           []string `json:"console"`
		Failed, OffOrigin []string
	}
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		t.Fatalf("the script printed no result (%v): %q", err, raw)
	}
	if !out.OK {
		t.Fatalf("the script failed: %s\nconsole: %q\nfailed requests: %q", out.Error, out.Console, out.Failed)
	}
	if out.Verdict != "ALL ASSERTIONS HOLD" {
		t.Errorf("verdict %q", out.Verdict)
	}
	if got := strings.Join(out.Outline, ","); got != "validate,charge,receipt" {
		t.Errorf("outline %q, want the nodes in config order", got)
	}
	if got := strings.Join(out.GraphNodes, ","); got != "validate,charge,receipt" {
		t.Errorf("graph nodes %q", got)
	}
	if out.SVGNamespace != "http://www.w3.org/2000/svg" {
		t.Errorf("the graph is in namespace %q, so the browser would not draw it", out.SVGNamespace)
	}
	if out.Source != testFlow {
		t.Errorf("source %q", out.Source)
	}
	if len(out.Assertions) != 2 || !strings.Contains(out.Assertions[0], "holds") {
		t.Errorf("assertions %q", out.Assertions)
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
