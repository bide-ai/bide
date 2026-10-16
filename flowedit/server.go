package flowedit

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bide-ai/bide/internal/runview"
)

// MaxFileSize is the largest config the editor reads, and the largest request body it accepts.
const MaxFileSize = 1 << 20

// TokenHeader is the request header that carries the session token on every /api call.
const TokenHeader = "X-Bide-Token"

//go:embed page
var pageFS embed.FS

// sharedAssets are the viewer's files the editor serves beside its own, by name: its design tokens,
// graph styles and theme script.
var sharedAssets = []string{"style.css", "graph.css", "extras.css", "theme.js"}

// assetTypes is the Content-Type of each file the editor serves.
var assetTypes = map[string]string{".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8"}

// ownAssets are the editor's own files in page/, by name.
var ownAssets = []string{"editor.js", "editor.css"}

var indexPage = template.Must(template.ParseFS(pageFS, "page/index.html"))

// contentSecurityPolicy is the strict set the approve page uses: nothing but same-origin script,
// style and fetch, no inline script or style, no framing, no base, no form posts.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Server is the editor for one config file. It holds the file's resolved path and the session
// token and nothing else: every request carries the text it works on, so there is no document, no
// session and no cache to invalidate on the server (docs/design/visual-editor.md, "The document
// lives in the browser").
type Server struct {
	path  string // the file, absolute, with symlinks resolved at startup
	name  string // the name the page shows: the path as given
	token string
	// baseline is the text every version is compared with for weakening, and baselineSource says
	// where it came from. They are fixed at startup: the baseline is the committed policy, not the
	// session, so weakening in two saves is still weakening against it.
	baseline       []byte
	baselineSource string
	baselineNote   string // why git HEAD was not the baseline, when that is not just "not in git"
}

// Option configures New.
type Option func(*options)

type options struct{ base string }

// WithBase compares edits with the file at the git revision rev (a branch, tag or commit, as `git
// show rev:./file` takes it) instead of at HEAD. New fails if the file is not there.
func WithBase(rev string) Option { return func(o *options) { o.base = rev } }

// New returns the editor for the file at path. The file must be an existing regular file of at
// most MaxFileSize bytes whose name ends in .yaml, .yml or .json; it is the only file the editor
// reads or writes. A symbolic link is resolved once, here.
func New(path string, opts ...Option) (*Server, error) {
	var o options
	for _, f := range opts {
		f(&o)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml", ".json":
	default:
		return nil, fmt.Errorf("flowedit: %s: a flow config is a .yaml, .yml or .json file", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("flowedit: %w", err)
	}
	if _, err := readConfig(real); err != nil {
		return nil, err
	}
	text, err := readConfig(real)
	if err != nil {
		return nil, err
	}
	// The baseline: the file at the revision asked for, else at HEAD when the file is committed,
	// else the file as it is now. The editor reads git only for this, with the same `git show` that
	// bide check -diff makes.
	dir, base := filepath.Split(real)
	var baseline []byte
	var source, note string
	switch {
	case o.base != "":
		if baseline, err = gitShow(dir, o.base, base); err != nil {
			return nil, fmt.Errorf("flowedit: -base: %w", err)
		}
		source = "git " + o.base
	default:
		if baseline, err = gitShow(dir, "HEAD", base); err == nil {
			source = "git HEAD"
		} else {
			baseline, source = text, "the file as it was when the editor opened it"
			// Only "the file is not in git" (an untracked file, not a repository) is an ordinary reason
			// to compare with the opened file. Anything else (git missing, a safe.directory refusal, a
			// corrupt repository) would quietly turn the guard into "weaker than what was opened", so
			// the reason is part of the baseline's name, which the page shows every time.
			if !gitNotThere(err) {
				source += " (git HEAD was not used: " + firstLine(err.Error()) + ")"
				note = source
			}
		}
	}
	var tok [16]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return nil, fmt.Errorf("flowedit: no randomness for the session token: %w", err)
	}
	return &Server{path: real, name: path, token: hex.EncodeToString(tok[:]), baseline: baseline, baselineSource: source, baselineNote: note}, nil
}

// BaselineNote is "" when the baseline is what it should be, and otherwise says why git HEAD was not
// used although the file may be in a repository; bide edit prints it at startup.
func (s *Server) BaselineNote() string { return s.baselineNote }

// Token is the session token the page sends in TokenHeader. bide edit prints it in the address's
// fragment (#t=...), which a browser does not send to the server or in a Referer.
func (s *Server) Token() string { return s.token }

// readConfig reads the file, which must be a regular file of at most MaxFileSize bytes.
// When id is not nil the opened file must be the file id describes (os.SameFile): the editor reads
// only the file it was started on, so a symbolic link or another file put at the path since is
// refused, not followed.
func readConfig(path string) ([]byte, error) {
	// Not a symbolic link or anything but a regular file at the name: a link put there since startup
	// is refused, not followed. A file another tool replaced by a rename is the file at the name now
	// and is read; the hash compare-and-swap of a save is what notices it changed.
	if lfi, err := os.Lstat(path); err != nil {
		return nil, fmt.Errorf("flowedit: %w", err)
	} else if lfi.Mode()&os.ModeSymlink != 0 || !lfi.Mode().IsRegular() {
		return nil, fmt.Errorf("flowedit: %s is not a regular file (a link or another kind of file is not read)", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("flowedit: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("flowedit: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("flowedit: %s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("flowedit: %w", err)
	}
	if len(b) > MaxFileSize {
		return nil, fmt.Errorf("flowedit: %s is larger than %d bytes", path, MaxFileSize)
	}
	return b, nil
}

// view is BuildView with the text compared against the baseline.
func (s *Server) view(text []byte) *View {
	v := BuildView(s.name, text)
	v.Weakening = Compare(s.name, s.baselineSource, s.baseline, text)
	return v
}

// Doc reads the file and returns its View with the hash of the bytes read.
func (s *Server) Doc() (*Doc, error) {
	b, err := readConfig(s.path)
	if err != nil {
		return nil, err
	}
	return &Doc{View: *s.view(b), Hash: hashOf(b)}, nil
}

// Handler serves the editor: the page at /, its files under /assets/, and the API under /api/. It
// answers only requests addressed to a loopback host, sets the same security headers on every
// response, and refuses an /api call that is not a POST of application/json from its own origin
// with the session token (see apiGuard). It is an http.Handler and nothing in it depends on a
// listener or the address, so another host can serve it.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /assets/{name}", s.asset)
	mux.Handle("POST /api/doc", s.apiGuard(s.doc))
	mux.Handle("POST /api/report", s.apiGuard(s.report))
	mux.Handle("POST /api/apply", s.apiGuard(s.apply))
	return secure(mux)
}

// Listen listens on addr, which must name a loopback IP or localhost, as bide view does.
func Listen(addr string) (net.Listener, error) { return runview.Listen(addr) }

// secure refuses a request that is not addressed to a loopback host (a DNS-rebinding page sends its
// own name) and sets the security headers on every response, errors included.
func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", contentSecurityPolicy)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		if !loopbackHost(r.Host) {
			http.Error(w, "bide edit answers only requests to a loopback address", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexPage.Execute(w, struct{ Sprite template.HTML }{template.HTML(runview.Sprite())}); err != nil { // the sprite is built from constants
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var b []byte
	var err error
	switch {
	case contains(sharedAssets, name):
		b, err = fs.ReadFile(runview.Assets(), name)
	case contains(ownAssets, name):
		b, err = fs.ReadFile(pageFS, "page/"+name)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// A fixed table, not the operating system's: with nosniff a platform whose registry maps .js to
	// text/plain would block the script and leave a dead page.
	w.Header().Set("Content-Type", assetTypes[filepath.Ext(name)])
	_, _ = w.Write(b)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// apiGuard admits an /api call only if it is application/json, its Origin is this server's own
// origin (an absent or foreign Origin is refused), it carries the session token, and its body is
// at most MaxFileSize bytes. The checks run in that order and each refusal is a JSON error.
func (s *Server) apiGuard(h func(w http.ResponseWriter, r *http.Request)) http.Handler {
	want := sha256.Sum256([]byte(s.token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			fail(w, http.StatusUnsupportedMediaType, "an API call is application/json")
			return
		}
		if r.Header.Get("Origin") != "http://"+r.Host {
			fail(w, http.StatusForbidden, "the request does not come from this page's origin")
			return
		}
		got := sha256.Sum256([]byte(r.Header.Get(TokenHeader)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			fail(w, http.StatusUnauthorized, "missing or wrong session token")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxFileSize)
		h(w, r)
	})
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// readBody reads the request's JSON body into v, refusing unknown fields and trailing data.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the request is larger than %d bytes", MaxFileSize))
			return false
		}
		fail(w, http.StatusBadRequest, "the request body is not the expected JSON: "+err.Error())
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "the request body has data after the JSON value")
		return false
	}
	return true
}

// doc answers /api/doc: the file as it is on disk now.
func (s *Server) doc(w http.ResponseWriter, r *http.Request) {
	if !readBody(w, r, &struct{}{}) {
		return
	}
	d, err := s.Doc()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, d)
}

// report answers /api/report: the View of the text in the request.
func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !readBody(w, r, &req) {
		return
	}
	reply(w, s.view([]byte(req.Text)))
}

// Edited is the answer to an operation: the View of the new text, and whether the operation changed
// the text (one that sets a value to what it already is does not).
type Edited struct {
	View
	Changed bool `json:"changed"`
}

// apply answers /api/apply: the operation applied to the text in the request. A refusal is a 200
// whose body is {"refused": reason}, and the page keeps the text it had.
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text      string    `json:"text"`
		Operation Operation `json:"operation"`
	}
	if !readBody(w, r, &req) {
		return
	}
	res, err := ApplyOperation(s.name, []byte(req.Text), req.Operation)
	var rf *Refusal
	switch {
	case errors.As(err, &rf):
		fail(w, http.StatusUnprocessableEntity, rf.Reason)
	case err != nil:
		fail(w, http.StatusInternalServerError, err.Error())
	default:
		reply(w, Edited{View: *s.view(res.Text), Changed: res.Changed})
	}
}
