package flowedit

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
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
		{"an unknown endpoint", "/api/save", "{}", nil, 404},
		{"body over the limit", "/api/report", `{"text":"` + strings.Repeat("a", MaxFileSize) + `"}`, nil, 413},
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
		if _, err := New(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if runtime.GOOS != "windows" { // creating a symbolic link needs a privilege on Windows
		target := write("real.yaml", testFlow)
		link := filepath.Join(dir, "link.yml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		s, err := New(link)
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
	v := BuildView("refund.yaml", []byte(testFlow))
	b, _ := json.Marshal(v)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"name", "format", "text", "mode", "flow", "entry", "verdict", "errors", "warnings", "assertions", "nodes", "wiring", "graph"} {
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
	for _, k := range []string{"name", "kind", "block", "safety", "risk", "gate"} {
		if _, ok := n[k]; !ok {
			t.Errorf("node has no %q", k)
		}
	}
	a := m["assertions"].([]any)[0].(map[string]any)
	for _, k := range []string{"text", "holds"} {
		if _, ok := a[k]; !ok {
			t.Errorf("assertion has no %q", k)
		}
	}
	js, _ := fs.ReadFile(pageFS, "page/editor.js")
	for _, k := range []string{"d.icon", "d.word", "d.line", "v.nodes", "v.wiring", "v.assertions", "v.problems", "v.graph", "v.parse_error", "v.verdict", "a.holds", "a.invalid", "a.undecided", "a.path", "a.message", "p.severity", "p.message", "n.gate", "n.risk"} {
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
