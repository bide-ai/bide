package approvals

import (
	"crypto/sha256"
	"crypto/sha512"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// The approve page is a static bundle: an entry document, one script and one stylesheet, with no
// dependency and no templating of server data. It holds an Ed25519 key in the browser (WebCrypto,
// not extractable), shows a pending call from the intake, and signs a decision on it. See
// docs/design/approve-page.md for what it does and does not protect against; the short version is
// that a page the application itself serves lets the application serve any script, so a deployment
// that matters hosts the bundle (PageFiles) on an origin of the approver's organization and pins it
// by the published hashes (PageSHA256).

//go:embed page/canon.js page/core.js page/flow.js page/app.js page/app.css page/index.template.html
var pageSource embed.FS

// pageModules are the script modules, in dependency order, that PageFiles joins into page.js.
var pageModules = []string{"canon", "core", "flow", "app"}

var (
	importLine = regexp.MustCompile(`(?m)^import \{([^}]*)\} from '\./(\w+)\.js';\n`)
	exportDecl = regexp.MustCompile(`(?m)^export (async function|function|const|class) (\w+)`)
)

// buildScript joins the modules into one script: each becomes a function scope that returns what it
// exports, and each import of another module becomes a destructuring of that scope's result, so no
// two modules share a name and no module is loaded separately (the entry document can pin one script
// by Subresource Integrity, which does not reach a module's imports). withApp false leaves the
// browser glue out, for the tests that run the logic under Node.
func buildScript(withApp bool) (string, error) {
	var b strings.Builder
	b.WriteString("'use strict';\n")
	for _, name := range pageModules {
		if name == "app" && !withApp {
			continue
		}
		src, err := pageSource.ReadFile("page/" + name + ".js")
		if err != nil {
			return "", err
		}
		body := string(src)
		body = importLine.ReplaceAllString(body, "")
		// Restore the imports as bindings of the modules they name.
		var binds strings.Builder
		for _, m := range importLine.FindAllStringSubmatch(string(src), -1) {
			fmt.Fprintf(&binds, "const {%s} = __%s;\n", strings.TrimSpace(m[1]), m[2])
		}
		var names []string
		for _, m := range exportDecl.FindAllStringSubmatch(body, -1) {
			names = append(names, m[2])
		}
		body = exportDecl.ReplaceAllString(body, "$1 $2")
		if strings.Contains(body, "\nexport ") || strings.HasPrefix(body, "export ") {
			return "", fmt.Errorf("approvals: page module %s has an export form buildScript does not read", name)
		}
		if name == "app" {
			fmt.Fprintf(&b, "(() => {\n%s%s})();\n", binds.String(), body)
			continue
		}
		fmt.Fprintf(&b, "const __%s = (() => {\n%s%s\nreturn { %s };\n})();\n", name, binds.String(), body, strings.Join(names, ", "))
	}
	return b.String(), nil
}

var sriRef = regexp.MustCompile(`\{\{sri ([\w.]+)\}\}`)

var (
	pageOnce  sync.Once
	pageFiles map[string][]byte
	pageErr   error
)

func buildPage() {
	script, err := buildScript(true)
	if err != nil {
		pageErr = err
		return
	}
	css, err := pageSource.ReadFile("page/app.css")
	if err != nil {
		pageErr = err
		return
	}
	tmpl, err := pageSource.ReadFile("page/index.template.html")
	if err != nil {
		pageErr = err
		return
	}
	files := map[string][]byte{"page.js": []byte(script), "app.css": css}
	html := sriRef.ReplaceAllStringFunc(string(tmpl), func(m string) string {
		name := sriRef.FindStringSubmatch(m)[1]
		sum := sha512.Sum384(files[name])
		return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
	})
	files["index.html"] = []byte(html)
	pageFiles = files
}

// PageFiles returns the files of the approve page by name: index.html, page.js and app.css. The
// build is deterministic: the same release always yields the same bytes, and index.html carries
// Subresource Integrity for the script and the stylesheet. A deployment that hosts the page itself
// serves these files (cmd/approvepage writes them with their SHA-256 sums) and may add a
// config.json naming the intake, {"intake": "https://app.example/approvals"}.
func PageFiles() map[string][]byte {
	pageOnce.Do(buildPage)
	if pageErr != nil {
		panic("approvals: building the approve page: " + pageErr.Error())
	}
	out := make(map[string][]byte, len(pageFiles))
	for k, v := range pageFiles {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// PageSHA256 returns the SHA-256 of each file of PageFiles, in lowercase hex: what a release
// publishes and a deployment verifies before it serves the page.
func PageSHA256() map[string]string {
	out := map[string]string{}
	for name, b := range PageFiles() {
		sum := sha256.Sum256(b)
		out[name] = hex.EncodeToString(sum[:])
	}
	return out
}

// pageCSP is the policy the page is served under by the application (WithPage): its own origin and
// nothing else, no inline script or style, no framing.
const pageCSP = "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// PageCSP returns the Content-Security-Policy a host of the page should send with it, for an
// intake at intakeOrigin ("" when the intake is the page's own origin): the policy WithPage serves,
// with connect-src naming the intake.
func PageCSP(intakeOrigin string) string {
	if intakeOrigin == "" {
		return pageCSP
	}
	return strings.Replace(pageCSP, "connect-src 'self'", "connect-src 'self' "+intakeOrigin, 1)
}

// WithPage makes the handler serve the approve page under /page/: an approver opens
// <mount>/page/#run=<id>&call=<id> and signs in the browser. It requires WithAuth (New returns an
// error without it): the page reads a call's arguments through GET /pending, so the endpoint must
// know who is asking, and the same hook authorizes the page's own files.
//
// The page is then served by the application, so the application can change what it does and can
// serve a script that signs anything the approver's key will sign; the page says so on its face.
// It is for development and for low-assurance use. For anything that matters, host PageFiles on an
// origin of the approver's organization (a dedicated one: IndexedDB and the key in it are scoped to
// the origin), pin the bundle by PageSHA256, and allow it with WithCORS. See
// docs/design/approve-page.md.
func WithPage() Option {
	return func(h *Handler) error { h.page = true; return nil }
}

// WithCORSCredentials lets the page send credentials cross-origin (fetch's credentials mode
// "include", which config.json asks for): cookies, HTTP authentication and, for mutual TLS, the
// browser's client certificate, which a browser does not present on a cross-origin request made with
// credentials omitted. It adds Access-Control-Allow-Credentials, which a browser accepts only with the
// exact origin WithCORS names. It requires WithCORS.
func WithCORSCredentials() Option {
	return func(h *Handler) error { h.corsCredentials = true; return nil }
}

// WithCORS lets a page hosted at origin (an exact origin such as "https://approve.example.com",
// never "*") call the intake from a browser: it answers the CORS preflight (OPTIONS) before any
// authentication, since a preflight carries no credentials and a browser can never reach an
// endpoint that refuses it, and adds the Access-Control headers to responses to that origin. It
// requires WithAuth. Credentials are not allowed cross-origin unless WithCORSCredentials says so; a
// deployment that authenticates with a cookie or a client certificate from another origin sets it,
// and the page's config.json asks for credentials "include". A deployment that needs neither serves
// the page from the intake's origin.
func WithCORS(origin string) Option {
	return func(h *Handler) error {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || strings.Contains(origin, "*") {
			return configErr("CORS origin %q is not an exact origin such as https://approve.example.com", origin)
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname())) {
			return configErr("CORS origin %q must be https (http only for localhost)", origin)
		}
		h.corsOrigin = u.Scheme + "://" + u.Host
		return nil
	}
}

func loopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// cors answers a CORS preflight, and marks responses to the allowed origin; it reports whether the
// request was a preflight it answered.
func (h *Handler) cors(w http.ResponseWriter, r *http.Request) bool {
	if h.corsOrigin == "" {
		return false
	}
	origin := r.Header.Get("Origin")
	w.Header().Add("Vary", "Origin")
	if origin != h.corsOrigin {
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent) // no Access-Control-Allow-Origin: the browser refuses the real request
			return true
		}
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	if h.corsCredentials {
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		w.Header().Add("Vary", "Access-Control-Request-Method")
		w.Header().Add("Vary", "Access-Control-Request-Headers")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

var pageTypes = map[string]string{"index.html": "text/html; charset=utf-8", "page.js": "text/javascript; charset=utf-8", "app.css": "text/css; charset=utf-8"}

// servePage serves a file of the page, under the page's headers.
func (h *Handler) servePage(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/page/")
	if name == "" {
		name = "index.html"
	}
	files := PageFiles()
	body, ok := files[name]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	if !h.authorize(w, r, Access{Method: r.Method}) {
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", pageTypes[name])
	hd.Set("Content-Security-Policy", pageCSP)
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cross-Origin-Opener-Policy", "same-origin")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
	hd.Set("X-Frame-Options", "DENY")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
