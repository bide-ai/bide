package flowedit

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/runview"
)

const testFlow = `# a refund flow
version: 1
flow: refund
entry: validate
nodes:
  - {name: validate, block: validate, safety: readonly}
  - name: charge
    block: charge
    safety: side_effect
    approval: {need: 2, approvers: [ops, finance, risk], exclude_principal: true}
  - {name: receipt, block: receipt, safety: idempotent}
wiring:
  - edge: [validate, charge]
  - edge: [charge, receipt]
assert:
  - every path to charge passes approval >= 2 of 3
  - receipt reachable
`

func newTestServer(t *testing.T, name, text string) (*Server, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t, p)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

// newServer is New for a test: the server's directory handle is closed when the test ends (Windows
// cannot remove a directory that is open, so a test's TempDir cleanup would fail).
func newServer(t *testing.T, path string, opts ...Option) (*Server, error) {
	t.Helper()
	s, err := New(path, opts...)
	if err == nil {
		t.Cleanup(func() { s.Close() })
	}
	return s, err
}

const host = "127.0.0.1:8484"

// call makes a request to h the way the page does, and lets a test change what is wrong with it.
func call(h http.Handler, s *Server, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://"+host+path, strings.NewReader(body))
	r.Host = host
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://"+host)
	r.Header.Set(TokenHeader, s.Token())
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDocAndReport(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	h := s.Handler()
	w := call(h, s, "/api/doc", "{}", nil)
	if w.Code != 200 {
		t.Fatalf("doc: %d %s", w.Code, w.Body)
	}
	var d Doc
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Text != testFlow || d.Hash != hashOf([]byte(testFlow)) || d.Format != "yaml" || d.Flow != "refund" || d.Entry != "validate" {
		t.Errorf("doc %+v", d.View)
	}
	var order []string
	for _, n := range d.Nodes {
		order = append(order, n.Name)
	}
	if strings.Join(order, ",") != "validate,charge,receipt" {
		t.Errorf("outline order %q: nodes are listed in config order, which the digest commits to", order)
	}
	if len(d.Nodes) != 3 || d.Nodes[1].Name != "charge" || d.Nodes[1].Gate != "2 of 3: ops, finance, risk excluding the requester" || d.Nodes[1].Safety != "side_effect" {
		t.Errorf("nodes %+v", d.Nodes)
	}
	if len(d.Wiring) != 2 || d.Wiring[0] != "validate -> charge" {
		t.Errorf("wiring %q", d.Wiring)
	}
	if len(d.Assertions) != 2 || !d.Assertions[0].Holds || !d.Assertions[1].Holds {
		t.Errorf("assertions %+v", d.Assertions)
	}
	if d.Verdict.Word != "ALL ASSERTIONS HOLD" || d.Verdict.Class != "good" || d.Mode != ModeLine {
		t.Errorf("verdict %+v, mode %q", d.Verdict, d.Mode)
	}
	if !strings.HasPrefix(d.Graph, "<svg") || !strings.Contains(d.Graph, `data-id="charge"`) {
		t.Errorf("graph %.80s", d.Graph)
	}

	// The report is of the text in the request, not of the file.
	broken := strings.Replace(testFlow, "  - receipt reachable\n", "  - no side effect after validate\n", 1)
	body, _ := json.Marshal(map[string]string{"text": broken})
	w = call(h, s, "/api/report", string(body), nil)
	var v View
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != 200 {
		t.Fatalf("report: %d %v %s", w.Code, err, w.Body)
	}
	if v.Text != broken || v.Verdict.Class != "bad" || !strings.Contains(v.Verdict.Word, "POLICY VIOLATION") || v.Assertions[1].Holds {
		t.Errorf("report verdict %+v assertions %+v", v.Verdict, v.Assertions)
	}
}

func TestReportParseFailure(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	for name, text := range map[string]string{
		"invalid YAML":       "nodes: [unclosed",
		"two documents":      testFlow + "---\nversion: 1\n",
		"duplicate key":      "version: 1\nversion: 1\n",
		"non-string key":     "version: 1\n? [a]\n: 1\n",
		"text after the doc": "{\"version\": 1}\n{\"version\": 1}\n",
	} {
		body, _ := json.Marshal(map[string]string{"text": text})
		w := call(s.Handler(), s, "/api/report", string(body), nil)
		var v View
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v.ParseError == "" || v.Verdict.Word != "CONFIG DOES NOT PARSE" || len(v.Nodes) != 0 || v.Graph != "" {
			t.Errorf("%s: %+v", name, v)
		}
	}
}

func TestJSONConfig(t *testing.T) {
	text := `{"version": 1, "flow": "f", "entry": "a", "nodes": [{"name": "a", "block": "a", "safety": "readonly"}], "wiring": []}`
	s, _ := newTestServer(t, "f.json", text)
	w := call(s.Handler(), s, "/api/doc", "{}", nil)
	var d Doc
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil || d.Format != "json" || d.ParseError != "" || len(d.Nodes) != 1 {
		t.Fatalf("%v %+v", err, d)
	}
}

// Every refusal of an /api call, one at a time: each is the only thing wrong with an otherwise
// good request, and none of them reaches the handler.
func TestAPIRefusals(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	h := s.Handler()
	for _, c := range []struct {
		name string
		path string
		body string
		mod  func(*http.Request)
		code int
	}{
		{"no token", "/api/doc", "{}", func(r *http.Request) { r.Header.Del(TokenHeader) }, 401},
		{"wrong token", "/api/doc", "{}", func(r *http.Request) { r.Header.Set(TokenHeader, strings.Repeat("0", 32)) }, 401},
		{"empty token", "/api/doc", "{}", func(r *http.Request) { r.Header.Set(TokenHeader, "") }, 401},
		{"token with a suffix", "/api/doc", "{}", func(r *http.Request) { r.Header.Set(TokenHeader, s.Token()+"0") }, 401},
		{"wrong Host", "/api/doc", "{}", func(r *http.Request) { r.Host = "evil.example:8484" }, 403},
		{"rebound Host with the right Origin", "/api/doc", "{}", func(r *http.Request) { r.Host = "evil.example"; r.Header.Set("Origin", "http://evil.example") }, 403},
		{"no Origin", "/api/doc", "{}", func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"foreign Origin", "/api/doc", "{}", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, 403},
		{"loopback Origin on another port", "/api/doc", "{}", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:9999") }, 403},
		{"https Origin", "/api/doc", "{}", func(r *http.Request) { r.Header.Set("Origin", "https://"+host) }, 403},
		{"null Origin", "/api/doc", "{}", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"form content type", "/api/doc", "{}", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, 415},
		{"text/plain content type", "/api/doc", "{}", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"no content type", "/api/doc", "{}", func(r *http.Request) { r.Header.Del("Content-Type") }, 415},
		{"GET", "/api/doc", "", func(r *http.Request) { r.Method = "GET" }, 405},
		{"PUT", "/api/doc", "{}", func(r *http.Request) { r.Method = "PUT" }, 405},
		{"an unknown endpoint", "/api/nope", "{}", nil, 404},
		{"body over the limit", "/api/report", `{"text":"` + strings.Repeat("a", MaxBodySize) + `"}`, nil, 413},
		{"text over the file limit", "/api/report", `{"text":"` + strings.Repeat("a", MaxFileSize+1) + `"}`, nil, 413},
		{"unknown field", "/api/doc", `{"path":"/etc/passwd"}`, nil, 400},
		{"not JSON", "/api/report", "text=a", nil, 400},
		{"data after the JSON", "/api/doc", "{} {}", nil, 400},
		{"text is not a string", "/api/report", `{"text":1}`, nil, 400},
	} {
		w := call(h, s, c.path, c.body, c.mod)
		if w.Code != c.code {
			t.Errorf("%s: status %d, want %d (%s)", c.name, w.Code, c.code, w.Body)
		}
		if strings.Contains(w.Body.String(), "refund") {
			t.Errorf("%s: the refusal carries the document", c.name)
		}
	}
	// The same requests with nothing wrong are served.
	if w := call(h, s, "/api/doc", "{}", nil); w.Code != 200 {
		t.Errorf("a good request: %d", w.Code)
	}
}

// Every response, an error or a file or the API, carries the policy that lets the page run nothing
// but its own files and makes no request off its origin.
func TestSecurityHeaders(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	h := s.Handler()
	get := func(path, hostHdr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://"+hostHdr+path, nil)
		r.Host = hostHdr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	responses := map[string]*httptest.ResponseRecorder{
		"index":           get("/", host),
		"stylesheet":      get("/assets/style.css", host),
		"script":          get("/assets/editor.js", host),
		"unknown asset":   get("/assets/secret.txt", host),
		"unknown page":    get("/nope", host),
		"bad host":        get("/", "evil.example"),
		"api ok":          call(h, s, "/api/doc", "{}", nil),
		"api no token":    call(h, s, "/api/doc", "{}", func(r *http.Request) { r.Header.Del(TokenHeader) }),
		"api wrong media": call(h, s, "/api/doc", "{}", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }),
		"api GET":         call(h, s, "/api/doc", "", func(r *http.Request) { r.Method = "GET" }),
	}
	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
	}
	for name, w := range responses {
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got, v)
			}
		}
	}
	if responses["index"].Code != 200 || responses["stylesheet"].Code != 200 || responses["script"].Code != 200 || responses["bad host"].Code != 403 {
		t.Errorf("statuses: index %d, css %d, js %d, bad host %d", responses["index"].Code, responses["stylesheet"].Code, responses["script"].Code, responses["bad host"].Code)
	}
}

// Only the files the editor ships are served, and the index carries the session's sprite, not the
// token (which only the printed address has).
func TestAssetsAndIndex(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	h := s.Handler()
	for _, name := range append(append([]string{}, sharedAssets...), ownAssets...) {
		r := httptest.NewRequest("GET", "http://"+host+"/assets/"+name, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("%s: %d, %d bytes", name, w.Code, w.Body.Len())
		}
	}
	for _, name := range []string{"view.js", "share.js", "config.html", "index.html", "..%2fserver.go", "%2e%2e%2fgo.mod", "editor.go"} {
		r := httptest.NewRequest("GET", "http://"+host+"/assets/"+name, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Errorf("/assets/%s: %d, want 404", name, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://"+host+"/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `id="ic-approval"`) || strings.Contains(body, s.Token()) {
		t.Errorf("index: sprite missing or token present")
	}
}

func TestNewRefusals(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := os.Mkdir(filepath.Join(dir, "d.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"a missing file":   filepath.Join(dir, "none.yaml"),
		"a directory":      filepath.Join(dir, "d.yaml"),
		"a wrong suffix":   write("flow.txt", testFlow),
		"no suffix":        write("flow", testFlow),
		"a file too large": write("big.yaml", "# "+strings.Repeat("a", MaxFileSize)+"\n"),
	} {
		if _, err := newServer(t, p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if runtime.GOOS != "windows" { // creating a symbolic link needs a privilege on Windows
		target := write("real.yaml", testFlow)
		link := filepath.Join(dir, "link.yml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		s, err := newServer(t, link)
		if err != nil {
			t.Fatal(err)
		}
		if want, _ := filepath.EvalSymlinks(target); s.path != want {
			t.Errorf("path %q, want the resolved %q", s.path, want)
		}
	}
}

// Config text is data. The page puts it in with textContent and element properties, so none of the
// ways to turn a string into markup or code appears in its script, and the HTML has no inline
// script, style or event handler (the policy would refuse them anyway).
func TestPageHasNoMarkupSinks(t *testing.T) {
	js, err := fs.ReadFile(pageFS, "page/editor.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setAttribute('on", `setAttribute("on`, "srcdoc", "createContextualFragment", "javascript:"} {
		if strings.Contains(string(js), sink) {
			t.Errorf("editor.js uses %s", sink)
		}
	}
	html, _ := fs.ReadFile(pageFS, "page/index.html")
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)<script(\s[^>]*)?>\s*[^<\s]`), // an inline script has a body
		regexp.MustCompile(`(?i)\sstyle\s*=`),
		regexp.MustCompile(`(?i)\son[a-z]+\s*=`),
		regexp.MustCompile(`(?i)<style`),
		regexp.MustCompile(`(?i)(src|href)\s*=\s*"https?:`),
	} {
		if m := bad.Find(html); m != nil {
			t.Errorf("index.html has %q", m)
		}
	}
}

// The page files are a budget the project chose (docs/design/visual-editor.md: 150 KB for all of
// them, the viewer's shared styles included), so growth is a visible decision.
func TestPageSizeBudget(t *testing.T) {
	const budget = 150 << 10
	total := 0
	for _, name := range ownAssets {
		b, err := fs.ReadFile(pageFS, "page/"+name)
		if err != nil {
			t.Fatal(err)
		}
		total += len(b)
	}
	b, _ := fs.ReadFile(pageFS, "page/index.html")
	total += len(b)
	for _, name := range sharedAssets {
		b, err := fs.ReadFile(runview.Assets(), name)
		if err != nil {
			t.Fatal(err)
		}
		total += len(b)
	}
	if total > budget {
		t.Errorf("page files are %d bytes, over the %d budget", total, budget)
	}
}

// The script reads these keys of the API's JSON. They are decoded from the Go structs, so renaming
// a field in Go without the script fails here.
func TestPageContract(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	b, _ := json.Marshal(s.view([]byte(testFlow)))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"name", "format", "text", "mode", "flow", "entry", "verdict", "errors", "warnings", "assertions", "nodes", "wiring", "graph", "weakening"} {
		if _, ok := m[k]; !ok {
			t.Errorf("View has no %q", k)
		}
	}
	vd := m["verdict"].(map[string]any)
	for _, k := range []string{"class", "word", "icon", "line", "stats"} {
		if _, ok := vd[k]; !ok {
			t.Errorf("verdict has no %q", k)
		}
	}
	n := m["nodes"].([]any)[1].(map[string]any)
	for _, k := range []string{"name", "kind", "block", "safety", "risk", "gate", "approval"} {
		if _, ok := n[k]; !ok {
			t.Errorf("node has no %q", k)
		}
	}
	ap := n["approval"].(map[string]any)
	for _, k := range []string{"need", "approvers", "exclude_principal"} {
		if _, ok := ap[k]; !ok {
			t.Errorf("approval has no %q", k)
		}
	}
	wk := m["weakening"].(map[string]any)
	if _, ok := wk["baseline"]; !ok {
		t.Errorf("weakening has no baseline")
	}
	a := m["assertions"].([]any)[0].(map[string]any)
	for _, k := range []string{"text", "holds"} {
		if _, ok := a[k]; !ok {
			t.Errorf("assertion has no %q", k)
		}
	}
	js, _ := fs.ReadFile(pageFS, "page/editor.js")
	for _, k := range []string{"d.icon", "d.word", "d.line", "v.nodes", "v.wiring", "v.assertions", "v.problems", "v.graph", "v.parse_error", "v.verdict", "a.holds", "a.invalid", "a.undecided", "a.path", "a.message", "p.severity", "p.message", "n.gate", "n.risk", "w.weakened", "w.word", "w.line", "w.baseline", "w.changes", "w.strengthened", "w.compare_error", "n.approval", "cur.need", "cur.approvers", "cur.exclude_principal", "cur.timeout", "n.safety", "n.block", "e.changed", "e.text"} {
		if !strings.Contains(string(js), k) {
			t.Errorf("the script no longer reads %s: update this list with it", k)
		}
	}
}

// The graph is parsed by the page as an XML document and adopted into it, so it must be well-formed
// XML in the SVG namespace, and node names (which are config text) must arrive escaped. The names
// below are valid to plan: a name is any string.
func TestGraphIsWellFormedAndEscaped(t *testing.T) {
	hostile := `<img src=x onerror=alert(1)>`
	text := "version: 1\nflow: f\nentry: a\nnodes:\n  - {name: 'a&b \"q\" ', block: '" + hostile + "', safety: readonly}\n  - {name: '" + hostile + "', block: b, safety: readonly}\nwiring:\n  - edge: ['a&b \"q\" ', '" + hostile + "']\n"
	v := BuildView("f.yaml", []byte(text))
	if v.Graph == "" || !strings.HasPrefix(v.Graph, `<svg xmlns="http://www.w3.org/2000/svg" `) {
		t.Fatalf("graph %.100s", v.Graph)
	}
	dec := xml.NewDecoder(strings.NewReader(v.Graph))
	elems := map[string]bool{}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the graph is not well-formed XML: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			elems[se.Name.Local] = true
			if se.Name.Space != "http://www.w3.org/2000/svg" {
				t.Errorf("element %s is in namespace %q", se.Name.Local, se.Name.Space)
			}
		}
	}
	if elems["img"] || elems["script"] {
		t.Errorf("a name became an element: %v", elems)
	}
	if strings.Contains(v.Graph, "<img") {
		t.Errorf("a name is in the graph unescaped")
	}
}

// The Content-Type of each asset is fixed in code (one charset, no operating-system lookup): with
// nosniff, a platform whose mime table maps .js to text/plain would block the module script.
func TestAssetContentTypes(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	for name, want := range map[string]string{
		"editor.js": "text/javascript; charset=utf-8", "theme.js": "text/javascript; charset=utf-8",
		"editor.css": "text/css; charset=utf-8", "style.css": "text/css; charset=utf-8", "graph.css": "text/css; charset=utf-8",
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://"+host+"/assets/"+name, nil))
		if got := w.Header().Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type %q, want %q", name, got, want)
		}
	}
}

// A symbolic link put at the config's name after startup is not followed on any read. A file another
// tool replaced by a rename (vim, an IDE, sed -i, git) is the file at the name now: it is read, and
// the hash compare-and-swap of a save is what notices it changed.
func TestReadsRefuseALinkButReadAReplacedFile(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", testFlow)
	secret := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(secret, []byte("TOPSECRET: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" { // creating a symbolic link needs a privilege there
		os.Remove(p)
		os.Symlink(secret, p)
		w := call(s.Handler(), s, "/api/doc", "{}", nil)
		if w.Code == 200 || strings.Contains(w.Body.String(), "TOPSECRET") {
			t.Errorf("a link: %d %s", w.Code, w.Body)
		}
	}
	// Replaced by a rename of another file over it: read, with the new text and hash.
	swp := p + ".swp"
	os.Remove(p)
	newer := strings.Replace(testFlow, "flow: refund", "flow: refund2", 1)
	if err := os.WriteFile(swp, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swp, p); err != nil {
		t.Fatal(err)
	}
	d, err := s.Doc()
	if err != nil || d.Text != newer || d.Hash != hashOf([]byte(newer)) {
		t.Fatalf("a replaced file is not read: %v", err)
	}
}

// A node name with a character XML 1.0 cannot hold does not blank the graph: the drawing stays
// well-formed and the name is shown with U+FFFD in its place.
func TestGraphSurvivesXMLIllegalNames(t *testing.T) {
	for _, bad := range []string{"\u0001", "\u000b", "\u001b", "￾", "￿"} {
		text := `{"version": 1, "flow": "f", "entry": "a` + bad + `b", "nodes": [{"name": "a` + bad + `b", "block": "x", "safety": "readonly"}], "wiring": []}`
		text = strings.ReplaceAll(text, bad, fmt.Sprintf(`\u%04x`, []rune(bad)[0]))
		v := BuildView("f.json", []byte(text))
		if v.Graph == "" {
			t.Fatalf("%q: no graph", bad)
		}
		dec := xml.NewDecoder(strings.NewReader(v.Graph))
		for {
			if _, err := dec.Token(); err != nil {
				if err != io.EOF {
					t.Errorf("%q: the graph is not well-formed: %v", bad, err)
				}
				break
			}
		}
		if !strings.Contains(v.Graph, "a�b") {
			t.Errorf("%q: the name is not shown with a marker", bad)
		}
	}
}

func applyReq(text string, op Operation) string {
	b, _ := json.Marshal(map[string]any{"text": text, "operation": op})
	return string(b)
}

func TestApplyEndpoint(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	h := s.Handler()
	w := call(h, s, "/api/apply", applyReq(testFlow, Operation{Op: "set_approval", Node: "receipt", Approval: &Approval{Need: 1, Approvers: []string{"ops"}, Timeout: "24h"}}), nil)
	var e Edited
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || w.Code != 200 {
		t.Fatalf("apply: %d %v %s", w.Code, err, w.Body)
	}
	if !e.Changed || !strings.Contains(e.Text, "- {name: receipt, block: receipt, safety: idempotent, approval: {need: 1, approvers: [ops], timeout: 24h}}") {
		t.Errorf("text:\n%s", e.Text)
	}
	if e.Nodes[2].Gate != "one of 1: ops, times out after 24h" && !strings.Contains(e.Nodes[2].Gate, "ops") {
		t.Errorf("the view is of the new text: gate %q", e.Nodes[2].Gate)
	}
	// A no-op changes nothing and says so; a refusal is a 422 that names why.
	w = call(h, s, "/api/apply", applyReq(testFlow, Operation{Op: "set_safety", Node: "validate", Value: "readonly"}), nil)
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || w.Code != 200 || e.Changed || e.Text != testFlow {
		t.Errorf("no-op: %d %v changed %v", w.Code, err, e.Changed)
	}
	for name, body := range map[string]string{
		"an unknown node":                       applyReq(testFlow, Operation{Op: "set_safety", Node: "nope", Value: "readonly"}),
		"an unknown operation":                  applyReq(testFlow, Operation{Op: "delete_node", Node: "charge"}),
		"a document the engine will not splice": applyReq(testFlow+"---\nversion: 1\n", Operation{Op: "set_safety", Node: "charge", Value: "readonly"}),
	} {
		w := call(h, s, "/api/apply", body, nil)
		if w.Code != 422 || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	// The same guards as every /api call.
	if w := call(h, s, "/api/apply", applyReq(testFlow, Operation{Op: "set_safety", Node: "charge", Value: "readonly"}), func(r *http.Request) { r.Header.Del(TokenHeader) }); w.Code != 401 {
		t.Errorf("no token: %d", w.Code)
	}
	if w := call(h, s, "/api/apply", `{"text":"x","operation":{"op":"set_safety","node":"a"},"path":"/etc/passwd"}`, nil); w.Code != 400 {
		t.Errorf("an unknown field: %d", w.Code)
	}
	// Nothing was written: the file is what it was.
	if d, err := s.Doc(); err != nil || d.Text != testFlow {
		t.Errorf("the file changed: %v", err)
	}
}

// Every route under /api is registered behind apiGuard (token, origin, content type, size). A new
// endpoint cannot skip it without this failing, and every one answers a request without the token
// with 401 and never reaches its handler.
func TestEveryAPIRouteIsGuarded(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "server.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") || len(call.Args) != 2 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || !strings.Contains(lit.Value, "/api/") {
			return true
		}
		route := strings.Trim(lit.Value, `"`)
		guarded := false
		if c, ok := call.Args[1].(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "apiGuard" {
				guarded = sel.Sel.Name == "Handle"
			}
		}
		if !guarded {
			t.Errorf("%s is not registered as mux.Handle(route, s.apiGuard(...))", route)
		}
		if !strings.HasPrefix(route, "POST ") {
			t.Errorf("%s is not a POST route", route)
		}
		routes = append(routes, strings.TrimPrefix(route, "POST "))
		return true
	})
	if len(routes) < 5 {
		t.Fatalf("found only %v: the route scan is broken", routes)
	}
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	for _, path := range routes {
		if w := call(s.Handler(), s, path, "{}", func(r *http.Request) { r.Header.Del(TokenHeader) }); w.Code != 401 {
			t.Errorf("%s without the token: %d", path, w.Code)
		}
		if w := call(s.Handler(), s, path, "{}", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); w.Code != 403 {
			t.Errorf("%s from a foreign origin: %d", path, w.Code)
		}
	}
}

// The script reads these keys of the preview and the save's refusals; they are decoded from the Go
// structs so renaming one in Go without the script fails here.
func TestPageContractForSave(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", weakFlow)
	pv, err := s.Preview([]byte(strings.Replace(weakFlow, "need: 2", "need: 1", 1)), hashOf([]byte(weakFlow)))
	if err != nil {
		t.Fatal(err)
	}
	pv.RemovedComments = []string{"# c"}
	pv.Conflict = true
	b, _ := json.Marshal(pv)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"diff", "removed_comments", "changed", "hash", "conflict", "needs_confirmation", "errors", "warnings"} {
		if _, ok := m[k]; !ok {
			t.Errorf("Preview has no %q", k)
		}
	}
	js, _ := fs.ReadFile(pageFS, "page/editor.js")
	for _, k := range []string{"pv.diff", "pv.removed_comments", "pv.changed", "pv.hash", "pv.conflict", "pv.needs_confirmation", "pv.errors", "e.data.hash", "e.data.diff", "d.text", "d.hash", "d.name"} {
		if !strings.Contains(string(js), k) {
			t.Errorf("the script no longer reads %s: update this list with it", k)
		}
	}
	// The refusals of a save carry these keys.
	w := call(s.Handler(), s, "/api/save", saveBody("nodes: [", hashOf([]byte(weakFlow))), nil)
	if w.Code != 422 || !strings.Contains(w.Body.String(), `"needs_confirmation"`) {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	if err := os.WriteFile(s.path, []byte(weakFlow+"# x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w = call(s.Handler(), s, "/api/save", saveBody("a: 1\n", hashOf([]byte(weakFlow))), nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"conflict":true`) || !strings.Contains(w.Body.String(), `"diff"`) || !strings.Contains(w.Body.String(), `"hash"`) {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

// The view carries the gate the config declares, which the forms edit, and survives a config whose
// gate is not a mapping: the check reports it and the node has no approval to edit.
func TestViewCarriesTheDeclaredGate(t *testing.T) {
	v := BuildView("refund.yaml", []byte(testFlow))
	ap := v.Nodes[1].Approval
	if ap == nil || ap.Need != 2 || !slices.Equal(ap.Approvers, []string{"ops", "finance", "risk"}) || !ap.ExcludePrincipal || ap.Timeout != "" {
		t.Errorf("%+v", ap)
	}
	if v.Nodes[0].Approval != nil || v.Nodes[2].Approval != nil {
		t.Errorf("a node with no gate has one: %+v %+v", v.Nodes[0].Approval, v.Nodes[2].Approval)
	}
	bad := strings.Replace(testFlow, "approval: {need: 2, approvers: [ops, finance, risk], exclude_principal: true}", "approval: yes", 1)
	v = BuildView("refund.yaml", []byte(bad))
	if v.ParseError != "" || len(v.Nodes) != 0 && v.Nodes[1].Approval != nil {
		t.Errorf("parse error %q, nodes %+v", v.ParseError, v.Nodes)
	}
	if v.Errors == 0 {
		t.Error("a gate that is not a mapping is not reported")
	}
}
