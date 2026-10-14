package approvals_test

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/approvals"
)

func allowAll(*http.Request, approvals.Access) error { return nil }

// The page needs to know who is asking: without WithAuth neither WithPage nor WithCORS builds.
func TestPage_OptionsNeedAuth(t *testing.T) {
	r := newRig(t)
	for name, opt := range map[string]approvals.Option{"WithPage": approvals.WithPage(), "WithCORS": approvals.WithCORS("https://approve.example.com")} {
		if _, err := approvals.New(r.app, r.verifiers, r.livePolicy(), opt); err == nil {
			t.Errorf("%s without WithAuth built", name)
		}
		if _, err := approvals.New(r.app, r.verifiers, r.livePolicy(), opt, approvals.WithAuth(allowAll)); err != nil {
			t.Errorf("%s with WithAuth: %v", name, err)
		}
	}
	for _, bad := range []string{"*", "https://*.example.com", "https://x.example.com/path", "https://x.example.com?q=1", "ftp://x", "http://example.com", "example.com", "https://u:p@x.example.com", ""} {
		if _, err := approvals.New(r.app, r.verifiers, r.livePolicy(), approvals.WithCORS(bad), approvals.WithAuth(allowAll)); err == nil {
			t.Errorf("CORS origin %q accepted", bad)
		}
	}
	if _, err := approvals.New(r.app, r.verifiers, r.livePolicy(), approvals.WithCORS("http://localhost:8080"), approvals.WithAuth(allowAll)); err != nil {
		t.Errorf("http localhost: %v", err)
	}
}

// The files are built the same every time, index.html pins the script and the stylesheet by
// Subresource Integrity, and the published sums are those of the files.
func TestPage_BundleIsDeterministicAndPinned(t *testing.T) {
	a, b := approvals.PageFiles(), approvals.PageFiles()
	if len(a) != 3 {
		t.Fatalf("files: %v", len(a))
	}
	for name := range a {
		if string(a[name]) != string(b[name]) {
			t.Errorf("%s differs between builds", name)
		}
	}
	html := string(a["index.html"])
	for _, file := range []string{"page.js", "app.css"} {
		sum := sha512.Sum384(a[file])
		want := `integrity="sha384-` + base64.StdEncoding.EncodeToString(sum[:]) + `"`
		if !strings.Contains(html, want) {
			t.Errorf("index.html does not carry the integrity of %s (%s)", file, want)
		}
	}
	if strings.Contains(html, "{{") || inlineScript(html) || strings.Contains(html, " style=") || strings.Contains(html, " onclick=") {
		t.Error("index.html has an unreplaced template, an inline script, or an inline style or handler")
	}
	sums := approvals.PageSHA256()
	for name, body := range a {
		if len(sums[name]) != 64 || sums[name] == "" {
			t.Errorf("sum of %s: %q", name, sums[name])
		}
		_ = body
	}
	if _, err := hex.DecodeString(sums["page.js"]); err != nil {
		t.Error(err)
	}
	// No third-party code and no network address in the script but the intake's own.
	if js := string(a["page.js"]); strings.Contains(js, "http://") || regexp.MustCompile(`https://\w`).MatchString(js) || strings.Contains(js, "innerHTML") || strings.Contains(js, "eval(") {
		t.Error("page.js reaches a fixed address, or uses innerHTML or eval")
	}
	if csp := approvals.PageCSP("https://app.example.com"); !strings.Contains(csp, "connect-src 'self' https://app.example.com;") || !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP: %s", csp)
	}
}

// Served by the handler, every file carries the page's headers and passes through the hook.
func TestPage_ServedUnderItsHeaders(t *testing.T) {
	var asked int
	r := newRig(t, approvals.WithPage(), approvals.WithAuth(func(req *http.Request, a approvals.Access) error {
		asked++
		if req.Header.Get("X-Client") == "" {
			return approvals.ErrUnauthenticated
		}
		return nil
	}))
	get := func(path string, hdr ...string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, r.srv.URL+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := get("/page/"); resp.StatusCode != 401 {
		t.Fatalf("without credentials: %d", resp.StatusCode)
	}
	for path, ctype := range map[string]string{"/page/": "text/html", "/page/index.html": "text/html", "/page/page.js": "text/javascript", "/page/app.css": "text/css"} {
		resp := get(path, "X-Client", "a")
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) {
			t.Fatalf("%s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		for h, want := range map[string]string{"Content-Security-Policy": "default-src 'none'", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"} {
			if !strings.Contains(resp.Header.Get(h), want) {
				t.Errorf("%s: header %s = %q", path, h, resp.Header.Get(h))
			}
		}
	}
	if resp := get("/page/nothing.js", "X-Client", "a"); resp.StatusCode != 404 {
		t.Fatalf("unknown file: %d", resp.StatusCode)
	}
	// Without WithPage there is no page.
	plain := newRig(t)
	resp, _ := http.Get(plain.srv.URL + "/page/")
	resp.Body.Close()
	if resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Fatalf("a handler without WithPage serves /page/: %d", resp.StatusCode)
	}
}

// The CORS preflight is answered before authentication (it carries no credentials), to the allowed
// origin only, while real requests are still authenticated and carry the allow header.
func TestCORS_PreflightBeforeAuth(t *testing.T) {
	const origin = "https://approve.example.com"
	r := newRig(t, approvals.WithCORS(origin), approvals.WithAuth(func(*http.Request, approvals.Access) error { return approvals.ErrUnauthenticated }),
		approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	do := func(method, path, from string, hdr ...string) *http.Response {
		req, _ := http.NewRequest(method, r.srv.URL+path, nil)
		if from != "" {
			req.Header.Set("Origin", from)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	pre := do(http.MethodOptions, "/decisions", origin, "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "content-type")
	if pre.StatusCode != 204 || pre.Header.Get("Access-Control-Allow-Origin") != origin || !strings.Contains(pre.Header.Get("Access-Control-Allow-Methods"), "POST") ||
		!strings.Contains(pre.Header.Get("Access-Control-Allow-Headers"), "Content-Type") || !strings.Contains(pre.Header.Get("Vary"), "Origin") {
		t.Fatalf("preflight: %d %v", pre.StatusCode, pre.Header)
	}
	if pre.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials allowed cross-origin")
	}
	other := do(http.MethodOptions, "/decisions", "https://evil.example.com", "Access-Control-Request-Method", "POST")
	if other.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("a foreign origin was allowed: %v", other.Header)
	}
	none := do(http.MethodOptions, "/decisions", "", "Access-Control-Request-Method", "POST")
	if none.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("a request with no origin was given CORS headers")
	}
	// A real request: still refused by the hook, and marked for the allowed origin so the page can read the refusal.
	real := do(http.MethodGet, "/pending?run="+runID+"&call=c1", origin)
	if real.StatusCode != 401 || real.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("real request: %d %v", real.StatusCode, real.Header)
	}
	if foreign := do(http.MethodGet, "/pending?run="+runID+"&call=c1", "https://evil.example.com"); foreign.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("a real request from a foreign origin was allowed")
	}
}

func nodeBin(t *testing.T) string {
	t.Helper()
	for _, c := range []string{"node", "/opt/homebrew/bin/node", "/usr/local/bin/node"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	if os.Getenv("CI") != "" {
		t.Fatal("node is not on PATH, and the approve page's script tests must run in CI (the workflow's test jobs install it)")
	}
	t.Skip("node is not installed: the page's script tests are skipped")
	return ""
}

// The script's own tests run under Node (node:test, no dependencies): canonical arguments and decision
// bytes against Go's vectors, the display rules, and the checks on the pending call.
func TestPage_ScriptTests(t *testing.T) {
	node := nodeBin(t)
	cmd := exec.Command(node, "--test", filepath.Join("page", "test")+string(filepath.Separator))
	files, _ := filepath.Glob(filepath.Join("page", "test", "*.test.mjs"))
	cmd = exec.Command(node, append([]string{"--test"}, files...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}

// The script the page ships (the modules joined into one) computes what the modules do: the joined
// script is run under Node against Go's vectors.
func TestPage_ShippedScriptMatchesTheModules(t *testing.T) {
	node := nodeBin(t)
	script, err := approvals.BuildScriptForTest(false)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bundle.js"), []byte(script+"\nglobalThis.__bundle = { canon: __canon, core: __core, flow: __flow };\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	check := `
const fs = require('fs'), vm = require('vm'), crypto = require('crypto');
const ctx = { TextEncoder, TextDecoder, crypto: crypto.webcrypto, URL, URLSearchParams, btoa, atob, console };
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(process.argv[1], 'utf8'), ctx);
const v = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const { canonicalArgs } = ctx.__bundle.canon;
let bad = 0;
for (const a of v.args) {
  const got = Buffer.from(canonicalArgs(new Uint8Array(Buffer.from(a.input_b64, 'base64')))).toString('base64');
  if (got !== a.canonical_b64) bad++;
}
if (bad) { console.log('differ ' + bad); process.exit(1); }
if (typeof ctx.__bundle.flow.decide !== 'function' || typeof ctx.__bundle.core.parsePending !== 'function') { console.log('missing exports'); process.exit(1); }
console.log('ok ' + v.args.length);
`
	cmd := exec.Command(node, "-e", check, filepath.Join(dir, "bundle.js"), filepath.Join("..", "agent", "testdata", "canonical_args_vectors.json"))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "ok ") {
		t.Fatalf("%v\n%s", err, out)
	}
	// The whole shipped script, browser glue included, is valid JavaScript.
	full, err := approvals.BuildScriptForTest(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "page.js"), []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", filepath.Join(dir, "page.js")).CombinedOutput(); err != nil {
		t.Fatalf("page.js does not parse: %v\n%s", err, out)
	}
}

type e2eOut struct {
	OK      bool   `json:"ok"`
	Refused bool   `json:"refused"`
	Error   string `json:"error"`
	Counted bool   `json:"counted"`
	Tool    string `json:"tool"`
	Digest  string `json:"digest"`
}

func (r *rig) runPage(t *testing.T, node, base, approver string, approved bool) e2eOut {
	t.Helper()
	priv := r.privs[approver]
	cmd := exec.Command(node, filepath.Join("page", "test", "e2e.mjs"))
	cmd.Env = append(os.Environ(), "BASE="+base, "RUN="+runID, "CALL=c1", "APPROVER="+approver, "APPROVED="+map[bool]string{true: "true", false: "false"}[approved],
		"SEED_HEX="+hex.EncodeToString(priv.Seed()), "PUB_HEX="+hex.EncodeToString(r.enrolled[approver]), "NOW="+r.clock().Add(time.Second).Format(time.RFC3339Nano))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node e2e: %v\n%s", err, out)
	}
	var o e2eOut
	line := strings.TrimSpace(string(out))
	if err := json.Unmarshal([]byte(line[strings.LastIndex(line, "\n")+1:]), &o); err != nil {
		t.Fatalf("e2e output %q: %v", out, err)
	}
	return o
}

// The page's decision flow, run under Node against a real intake: two approvers decide through it,
// their signatures verify at the gate, the call runs once, and each receipt verified under the key.
func TestPage_FlowAgainstARealIntake(t *testing.T) {
	node := nodeBin(t)
	r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	for _, id := range []string{"alice", "bob"} {
		o := r.runPage(t, node, r.srv.URL, id, true)
		if !o.OK || !o.Counted || o.Tool != "charge" || len(o.Digest) != 64 {
			t.Fatalf("%s: %+v", id, o)
		}
	}
	if err := r.run(); err != nil || r.charged.Load() != 1 {
		t.Fatalf("run at quorum: %v (charged %d)", err, r.charged.Load())
	}
	// The gate counted both: the decisions the page signed verify under the enrolled keys.
	_, pend := r.get("/pending?run=" + runID + "&call=c1")
	if gate := pend["gate"].(map[string]any); gate["approved"] != float64(2) || gate["passed"] != true {
		t.Fatalf("gate %v", gate)
	}
}

// An endpoint that shows one tool and then another gets nothing signed from the page, and a
// rejection is signed as one.
func TestPage_RefusesWhatChangesBetweenDisplayAndSignature(t *testing.T) {
	node := nodeBin(t)
	r := newRig(t, approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	gets := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := httptest.NewRecorder()
		r.h.ServeHTTP(rec, req)
		body := rec.Body.Bytes()
		if req.Method == http.MethodGet {
			gets++
			if gets > 1 {
				body = []byte(strings.Replace(string(body), `"tool":"charge"`, `"tool":"lookup"`, 1))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Code)
		w.Write(body)
	}))
	defer proxy.Close()
	before := r.records()
	o := r.runPage(t, node, proxy.URL, "alice", true)
	if o.OK || !o.Refused || !strings.Contains(o.Error, "changed while you were deciding") || r.records() != before {
		t.Fatalf("%+v (records %d, was %d)", o, r.records(), before)
	}
	rej := r.runPage(t, node, r.srv.URL, "bob", false)
	if !rej.OK {
		t.Fatalf("%+v", rej)
	}
	recs, _ := r.app.History(t.Context(), runID)
	found := false
	for _, rc := range recs {
		if rc.Kind == agent.StepApproval && rc.Approver() == "bob" && !rc.Approved {
			found = true
		}
	}
	if !found {
		t.Fatal("the page's rejection was not recorded as one")
	}
}

// inlineScript reports a <script> element with no src attribute.
func inlineScript(html string) bool {
	for _, m := range regexp.MustCompile(`<script[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, " src=") {
			return true
		}
	}
	return false
}

// The page's routes answer a stalled body like every other path (WithPage on).
func TestPage_EveryPathAnswersAStalledBody(t *testing.T) {
	r := newRig(t, approvals.WithPage(), approvals.WithAuth(allowAll), approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	var wg sync.WaitGroup
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS"} {
		for _, path := range []string{"/page/", "/page/page.js", "/page/app.css", "/page/nothing.js", "/page/config.json"} {
			wg.Go(func() {
				if got := stalledStatus(t, r, method, path, "text/plain"); got == "" {
					t.Errorf("%s %s with a stalled body: no response within 700ms", method, path)
				}
			})
		}
	}
	wg.Wait()
}
