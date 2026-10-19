package approvals_test

import (
	"bufio"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
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
	// The DOM tests run the script the page ships.
	script, err := approvals.BuildScriptForTest(true)
	if err != nil {
		t.Fatal(err)
	}
	pageJS := filepath.Join(t.TempDir(), "page.js")
	if err := os.WriteFile(pageJS, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "PAGE_JS="+pageJS)
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

// The members the page accepts of GET /pending are the json tags of approvals.Pending: a field added
// to the endpoint's answer and not to the page (PENDING_KEYS) would make the page refuse every call.
func TestPage_PendingKeysAreTheStructsTags(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("page", "core.js"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const PENDING_KEYS = new Set\(\[([^\]]*)\]\)`).FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("core.js has no PENDING_KEYS")
	}
	var js []string
	for _, q := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(m[1], -1) {
		js = append(js, q[1])
	}
	var tags []string
	rt := reflect.TypeFor[approvals.Pending]()
	for f := range rt.Fields() {
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
			tags = append(tags, name)
		}
	}
	slices.Sort(js)
	slices.Sort(tags)
	if !slices.Equal(js, tags) {
		t.Fatalf("core.js PENDING_KEYS %v, approvals.Pending tags %v", js, tags)
	}
}

// What the handler really sends, in every state it can send it in, is accepted by the page's parser:
// the answers are written to files and parsed under Node.
func TestPage_ParsesEveryPendingAnswerTheHandlerGives(t *testing.T) {
	node := nodeBin(t)
	dir := t.TempDir()
	save := func(name string, r *rig, run string) {
		resp, err := http.Get(r.srv.URL + "/pending?run=" + run + "&call=c1")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", name, resp.StatusCode, b)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	off := []approvals.Option{approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0)}
	// waiting, with decisions in it
	w := newRig(t, off...)
	w.post(w.decision("alice", true))
	save("waiting", w, runID)
	// decided
	d := newRig(t, off...)
	d.post(d.decision("alice", true))
	d.post(d.decision("bob", true))
	if err := d.run(); err != nil {
		t.Fatal(err)
	}
	save("decided", d, runID)
	// re-asked, with a reason
	a := newRigAs(t, "c1", &agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob", "carol"}}, agent.Identity{Actor: "x", OnBehalfOf: "alice"}, off...)
	a.policy.ExcludePrincipal = true
	raw, _ := json.Marshal(agent.ApprovalTally{Need: 2, Approvers: a.policy.Approvers, Approved: 2, ApprovedBy: []string{"alice", "bob"}, DecisionFormat: agent.DecisionFormat})
	data, _ := agent.JournalEntry(agent.ApprovalTallyStep("c1"), agent.Record{Kind: agent.StepValue, Result: raw})
	if _, _, err := a.stall.Store.Insert(context.Background(), runID, agent.ApprovalTallyStep("c1"), data); err != nil {
		t.Fatal(err)
	}
	save("reasked", a, runID)
	// misconfigured (error, with a reason)
	e := newRig(t, off...)
	e.policy = agent.ApprovalPolicy{Need: 5, Approvers: []string{"alice", "bob"}}
	save("error", e, runID)
	// single
	s := newRig(t, off...)
	s.single = true
	save("single", s, runID)
	script := `
const fs = require('fs');
import('` + fileURL(mustAbs(t, filepath.Join("page", "core.js"))) + `').then(({ parsePending }) => {
  let bad = 0;
  for (const f of process.argv.slice(1)) {
    try { parsePending(new Uint8Array(fs.readFileSync(f))); } catch (e) { bad++; console.log(f + ': ' + e.message); }
  }
  process.exit(bad ? 1 : 0);
});`
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 5 {
		t.Fatalf("%d files", len(files))
	}
	if out, err := exec.Command(node, append([]string{"-e", script}, files...)...).CombinedOutput(); err != nil {
		t.Fatalf("the page's parser refuses an answer the handler gives: %v\n%s", err, out)
	}
	// And the reasoned ones carry a reason.
	for _, name := range []string{"reasked", "error"} {
		b, _ := os.ReadFile(filepath.Join(dir, name+".json"))
		if !strings.Contains(string(b), `"reason"`) {
			t.Errorf("%s has no reason: %s", name, b)
		}
	}
}

// fileURL is the file: URL of an absolute path, which Node's ES module loader needs on Windows (a
// drive letter is not a scheme it accepts).
func fileURL(abs string) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The CORS preflight is limited like everything else, answered without waiting for a body it
// does not read, and still before authentication.
func TestCORS_PreflightIsLimitedAndAnsweredAtOnce(t *testing.T) {
	const origin = "https://approve.example.com"
	r := newRig(t, approvals.WithAuth(func(*http.Request, approvals.Access) error { return approvals.ErrUnauthenticated }),
		approvals.WithCORS(origin), approvals.WithRateLimit(0.001, 3), approvals.WithGlobalRateLimit(0, 0))
	preflight := func() int {
		req, _ := http.NewRequest("OPTIONS", r.srv.URL+"/decisions", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := range 3 {
		if got := preflight(); got != 204 {
			t.Fatalf("preflight %d: %d", i+1, got)
		}
	}
	if got := preflight(); got != http.StatusTooManyRequests {
		t.Fatalf("a fourth preflight from one client at a burst of 3: %d, want 429", got)
	}
	// A preflight whose body stalls is answered at once.
	r2 := newRig(t, approvals.WithAuth(allowAll), approvals.WithCORS(origin), approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0))
	c, err := net.Dial("tcp", r2.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("OPTIONS /decisions HTTP/1.1\r\nHost: x\r\nOrigin: " + origin + "\r\nAccess-Control-Request-Method: POST\r\nContent-Length: 4000\r\n\r\n{"))
	c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	if line, _ := bufio.NewReader(c).ReadString('\n'); !strings.Contains(line, "204") {
		t.Fatalf("a preflight with a stalled body: %q, want an immediate 204", line)
	}
}

// Credentials cross-origin are allowed only when asked for, to the one origin, and need WithCORS.
func TestCORS_CredentialsOnlyWhenAskedFor(t *testing.T) {
	const origin = "https://approve.example.com"
	for _, creds := range []bool{false, true} {
		opts := []approvals.Option{approvals.WithAuth(allowAll), approvals.WithCORS(origin), approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0)}
		if creds {
			opts = append(opts, approvals.WithCORSCredentials())
		}
		r := newRig(t, opts...)
		req, _ := http.NewRequest("OPTIONS", r.srv.URL+"/decisions", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Credentials"); (got == "true") != creds {
			t.Errorf("credentials %v: Access-Control-Allow-Credentials %q", creds, got)
		}
		req2, _ := http.NewRequest("GET", r.srv.URL+"/pending?run="+runID+"&call=c1", nil)
		req2.Header.Set("Origin", "https://evil.example.com")
		resp2, _ := http.DefaultClient.Do(req2)
		resp2.Body.Close()
		if resp2.Header.Get("Access-Control-Allow-Credentials") != "" || resp2.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Error("a foreign origin was allowed, with credentials")
		}
	}
	r := newRig(t)
	if _, err := approvals.New(r.app, r.verifiers, r.livePolicy(), approvals.WithCORSCredentials()); err == nil {
		t.Error("WithCORSCredentials without WithCORS built")
	}
}

// The page's look is the shipped themes in its pinned bundle, picked by the system's settings alone
// (docs/design/themes.md, option B): nothing a script, a URL, config.json or the host supplies can
// choose, restyle or brand it, so a host cannot change what an approver sees without changing a
// pinned file. The bundle's stylesheet is the generated themes in front of the page's own rules, with
// no theme selector, no colour of its own, and no way into config.json or the script.
func TestPage_TakesNoThemeInput(t *testing.T) {
	files := approvals.PageFiles()
	css, js, html := string(files["app.css"]), string(files["page.js"]), string(files["index.html"])
	if !strings.HasPrefix(css, "/* Generated by bide from the checked themes. Do not edit. */\n:root {") {
		t.Error("app.css does not start with the generated themes")
	}
	for _, bad := range []string{"data-theme", "--bide-", "@import", "url(", "attr(", "image-set(", "content:", "@font-face", "@namespace", "@property", "@layer", "@container"} {
		if strings.Contains(css, bad) {
			t.Errorf("app.css has %q: something outside the system's settings could pick, load or add to the look", bad)
		}
	}
	for _, bad := range []string{"data-theme", "bide-theme", "localStorage", "sessionStorage", "setProperty", "insertRule", "adoptedStyleSheets", "createElement('style')", "createElement('link')", "styleSheets", ".style.", "cssText"} {
		if strings.Contains(js, bad) {
			t.Errorf("page.js has %q: the page's script must not restyle it", bad)
		}
	}
	if strings.Contains(strings.ToLower(js), "theme") {
		t.Error("page.js mentions a theme: it must take none")
	}
	// config.json names the intake and the credentials mode, and nothing about the look: the script
	// reads exactly those two keys of it.
	fn := js[strings.Index(js, "async function intakeBase()"):]
	fn = fn[:strings.Index(fn, "function fragment()")]
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bc\.(\w+)`).FindAllStringSubmatch(fn, -1) {
		keys[m[1]] = true
	}
	if len(keys) != 2 || !keys["intake"] || !keys["credentials"] {
		t.Errorf("the page reads %v of config.json, want only intake and credentials", keys)
	}
	// The only stylesheet is the pinned one; no inline style and no link to another.
	if n := strings.Count(html, "<link"); n != 1 || !strings.Contains(html, `href="app.css" integrity="sha384-`) || strings.Contains(html, "<style") || strings.Contains(html, " style=") {
		t.Errorf("index.html loads more than the one pinned stylesheet (%d links)", n)
	}
	// The page's own rules are the source file, after the generated themes, and hold no colour literal:
	// every color is a theme's. (The source is read as it is, so a forced-colors block of its own, or any
	// other, cannot hide rules from the check.)
	src, err := os.ReadFile("page/app.css")
	if err != nil {
		t.Fatal(err)
	}
	own := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.HasSuffix(css, own) {
		t.Error("app.css does not end with the page's own rules")
	}
	if regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(|\b(?:white|black|red|green|blue|gray|grey)\b\s*[;}]`).MatchString(own) {
		t.Errorf("the page's own rules hold a colour literal:\n%s", own)
	}
	if len(own) < 500 {
		t.Errorf("the page's own rules are %d bytes", len(own))
	}
}

// The approve bundle is pinned by hash in every release, and by organisations that host it: any change to
// it, a page edit or a change to a shipped theme (which is written into app.css), is a deliberate change
// to a signed artefact. The golden below is the bundle's SHA256SUMS; a change that moves it must be
// reviewed as that, and the golden updated with the change (go test ./approvals -run
// TestPage_BundleHashesAreReviewed -update).
var updateBundleGolden = flag.Bool("update", false, "rewrite testdata/approve-page.SHA256SUMS")

func TestPage_BundleHashesAreReviewed(t *testing.T) {
	sums := approvals.PageSHA256()
	var names []string
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", sums[n], n)
	}
	const golden = "testdata/approve-page.SHA256SUMS"
	if *updateBundleGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (go test ./approvals -run TestPage_BundleHashesAreReviewed -update)", err)
	}
	if string(want) != b.String() {
		t.Errorf("the approve page bundle changed: its SHA256SUMS are now\n%swas\n%s\nThis is a change to the signed bundle every release publishes and organisations pin: a page edit, or a change to a shipped theme. If it is intended, update the golden (go test ./approvals -run TestPage_BundleHashesAreReviewed -update) and say so in the pull request and the changelog.", b.String(), want)
	}
}
