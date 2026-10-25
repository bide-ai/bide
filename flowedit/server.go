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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/internal/editrun"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/safefile"
	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/theme"
)

// MaxFileSize is the largest config the editor reads, and the largest request body it accepts.
const MaxFileSize = 1 << 20

// MaxBodySize is the largest request body: a config of MaxFileSize bytes is a JSON string in it,
// where every newline, quote and tab takes two bytes and a control character six, so the body of a
// maximal valid config is up to six times the file. The text itself is checked against MaxFileSize
// after it is decoded.
const MaxBodySize = 6*MaxFileSize + 4096

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
	mu    sync.Mutex // one read or save at a time; guards id
	root  *os.Root   // the config's directory, opened at startup
	base  string     // the config's name in it
	// baseline is the text every version is compared with for weakening, and baselineSource says
	// where it came from. They are fixed at startup: the baseline is the committed policy, not the
	// session, so weakening in two saves is still weakening against it.
	baseline       []byte
	baselineSource string
	baselineNote   string       // why git HEAD was not the baseline, when that is not just "not in git"
	fs             fileSystem   // what a save writes with; a test replaces it
	typing         *typing      // nil: the structural editor
	run            *editrun.Run // the run the file is compared with (bide edit -run), nil for none
	runs           runState     // the one snapshot of it
	aff            scanState    // the one scan of the store's unfinished runs
	opened         []byte       // the file as it was when the editor opened it
	assets         fs.FS        // the viewer's files the editor serves, with the host's themes in style.css; nil: the shipped themes
}

// Option configures New.
type Option func(*options)

type options struct {
	base     string
	reg      *plan.Registry
	regGiven bool // WithRegistry was given, even with nil
	blocks   *plan.RegistryDescription
	themes   runview.ThemeSet
	run      *editrun.Run
}

// WithBase compares edits with the file at the git revision rev (a branch, tag or commit, as `git
// show rev:./file` takes it) instead of at HEAD. New fails if the file is not there.
func WithBase(rev string) Option { return func(o *options) { o.base = rev } }

// WithThemes adds custom themes, each from theme.Parse or theme.Load (so each passed the checks in
// docs/design/themes.md), beside the shipped light, dark and high-contrast. A host passes values,
// never CSS: the stylesheet is generated from the checked values alone. New fails when a theme is
// nil or not from Parse or Load, or when two themes (a shipped one included) have one name. The
// page's switch steps through the shipped themes and these.
func WithThemes(themes ...*theme.Theme) Option {
	return func(o *options) { o.themes.Extra = append(o.themes.Extra, themes...) }
}

// WithDefaultTheme sets the theme the page starts in, by name: a shipped theme or one added with
// WithThemes. A reader who asks their system for more contrast or forced colors gets those, over
// the default. New fails when the name is not a theme in the set or differs from WithThemeLock's.
func WithDefaultTheme(name string) Option { return func(o *options) { o.themes.Default = name } }

// WithThemeLock makes the named theme the only one the page has: the page's switch is hidden, a
// remembered choice is ignored, and the reader's system setting no longer picks light or dark; a
// reader who asks their system for more contrast or forced colors still gets those, over the lock. New fails when the name is not a theme in the set or differs from
// WithDefaultTheme's.
func WithThemeLock(name string) Option { return func(o *options) { o.themes.Lock = name } }

// New returns the editor for the file at path. The file must be an existing regular file of at
// most MaxFileSize bytes whose name ends in .yaml, .yml or .json; it is the only file the editor
// reads or writes. A symbolic link is resolved once, here.
func New(path string, opts ...Option) (*Server, error) {
	var o options
	for _, f := range opts {
		f(&o)
	}
	ty, err := newTyping(o)
	if err != nil {
		return nil, err
	}
	if err := o.themes.Validate(); err != nil {
		return nil, fmt.Errorf("flowedit: themes: %w", err)
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
	dir, base := filepath.Split(real)
	// The directory is opened once, here: every read and write is through this handle, so a
	// directory swapped for a link later cannot redirect them.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("flowedit: %w", err)
	}
	fail := func(err error) (*Server, error) { root.Close(); return nil, err }
	if _, err := readConfig(root, base, real); err != nil {
		return fail(err)
	}
	text, err := readConfig(root, base, real)
	if err != nil {
		return fail(err)
	}
	cleanStale(root, time.Now())
	// The baseline: the file at the revision asked for, else at HEAD when the file is committed,
	// else the file as it is now. The editor reads git only for this, with the same `git show` that
	// bide check -diff makes.
	var baseline []byte
	var source, note string
	switch {
	case o.base != "":
		if baseline, err = gitShow(dir, o.base, base); err != nil {
			return fail(fmt.Errorf("flowedit: -base: %w", err))
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
		return fail(fmt.Errorf("flowedit: no randomness for the session token: %w", err))
	}
	return &Server{path: real, root: root, base: base, name: path, token: hex.EncodeToString(tok[:]), baseline: baseline, baselineSource: source, baselineNote: note, fs: osFS, typing: ty, run: o.run, opened: text, assets: runview.AssetsWith(o.themes)}, nil
}

// BaselineNote is "" when the baseline is what it should be, and otherwise says why git HEAD was not
// used although the file may be in a repository; bide edit prints it at startup.
func (s *Server) BaselineNote() string { return s.baselineNote }

// Close releases the directory handle. A Server must not be used after.
func (s *Server) Close() error { return s.root.Close() }

// Token is the session token the page sends in TokenHeader. bide edit prints it in the address's
// fragment (#t=...), which a browser does not send to the server or in a Referer.
func (s *Server) Token() string { return s.token }

// afterLstat is a test hook run between readConfig's check of the name and its open, so a test can
// swap the file there. It is nil outside tests.
var afterLstat func()

// readConfig reads the file, which must be a regular file of at most MaxFileSize bytes.
func readConfig(root *os.Root, base, path string) ([]byte, error) {
	// Not a symbolic link or anything but a regular file at the name: a link put there since startup
	// is refused, not followed. A file another tool replaced by a rename is the file at the name now
	// and is read; the hash compare-and-swap of a save is what notices it changed.
	if lfi, err := root.Lstat(base); err != nil {
		return nil, fmt.Errorf("flowedit: %w", err)
	} else if lfi.Mode()&os.ModeSymlink != 0 || !lfi.Mode().IsRegular() {
		return nil, fmt.Errorf("flowedit: %s is not a regular file (a link or another kind of file is not read)", path)
	}
	if afterLstat != nil {
		afterLstat()
	}
	f, err := safefile.OpenIn(root, base) // not waiting for a writer, were the name swapped for a named pipe
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
func (s *Server) view(text []byte) *View { return s.viewRenamed(text, nil) }

// viewRenamed is view for a text reached by the session's renames.
func (s *Server) viewRenamed(text []byte, renames []Rename) *View {
	v := buildView(s.name, text, s.typing)
	v.Weakening = s.compareRenamed(text, renames)
	return v
}

// Doc reads the file and returns its View with the hash of the bytes read.
func (s *Server) Doc() (*Doc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docLocked()
}

func (s *Server) docLocked() (*Doc, error) { return s.docRenamed(nil) }

// docRenamed is docLocked with the verdict read under the session's renames, so a save of a renamed
// node does not come back reading as weakened.
func (s *Server) docRenamed(renames []Rename) (*Doc, error) {
	b, err := readConfig(s.root, s.base, s.path)
	if err != nil {
		return nil, err
	}
	return &Doc{View: *s.viewRenamed(b, renames), Hash: hashOf(b)}, nil
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
	mux.Handle("POST /api/preview", s.apiGuard(s.preview))
	mux.Handle("POST /api/save", s.apiGuard(s.save))
	mux.Handle("POST /api/run", s.apiGuard(s.serveRun))
	mux.Handle("POST /api/affected", s.apiGuard(s.serveAffected))
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
		b, err = fs.ReadFile(s.sharedFS(), name)
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

// sharedFS is the viewer's files with the themes the host chose.
func (s *Server) sharedFS() fs.FS {
	if s.assets == nil {
		return runview.Assets()
	}
	return s.assets
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
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodySize)
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
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the request is larger than %d bytes", MaxBodySize))
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
		Text    string   `json:"text"`
		Renames []Rename `json:"renames"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if len(req.Text) > MaxFileSize {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the text is larger than %d bytes", MaxFileSize))
		return
	}
	reply(w, s.viewRenamed([]byte(req.Text), req.Renames))
}

// Edited is the answer to an operation: the View of the new text, and whether the operation changed
// the text (one that sets a value to what it already is does not).
type Edited struct {
	View
	Changed bool `json:"changed"`
	// Cascade is what a remove_node also removes (Applied.Cascade); a dry run answers with it alone.
	Cascade []string `json:"cascade,omitempty"`
	// Renames is the session's renames after this operation: the ones the request carried, and this
	// operation's when it was a rename that changed the text. The page keeps the list with the text
	// (an undo restores the list it had) and sends it back with every request.
	Renames []Rename `json:"renames,omitempty"`
}

// apply answers /api/apply: the operation applied to the text in the request. A refusal is not an
// HTTP error (the page asks what an operation would do as a question, and a browser logs a failed
// request as an error): it is a 200 whose body is {"refused": reason}, and the page keeps the text it
// had.
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text      string    `json:"text"`
		Operation Operation `json:"operation"`
		Renames   []Rename  `json:"renames"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if len(req.Text) > MaxFileSize {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the text is larger than %d bytes", MaxFileSize))
		return
	}
	res, err := ApplyOperation(s.name, []byte(req.Text), req.Operation)
	var rf *Refusal
	if err == nil && res.Changed {
		if why := s.typedRefusal([]byte(req.Text), []byte(res.Text), req.Operation); why != "" {
			err = &Refusal{Reason: why}
		}
	}
	switch {
	case errors.As(err, &rf):
		reply(w, map[string]string{"refused": rf.Reason})
	case err != nil:
		fail(w, http.StatusInternalServerError, err.Error())
	default:
		renames := req.Renames
		if res.Changed && req.Operation.Op == "rename_node" {
			renames = append(slices.Clone(renames), Rename{From: req.Operation.Node, To: req.Operation.Value})
		}
		reply(w, Edited{View: *s.viewRenamed([]byte(res.Text), renames), Changed: res.Changed, Cascade: res.Cascade, Renames: renames})
	}
}

// preview answers /api/preview: what saving the text would do (see Preview).
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text    string   `json:"text"`
		Hash    string   `json:"hash"`
		Renames []Rename `json:"renames"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if len(req.Text) > MaxFileSize {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the text is larger than %d bytes", MaxFileSize))
		return
	}
	p, err := s.previewRenamed([]byte(req.Text), req.Hash, req.Renames)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	reply(w, p)
}

// save answers /api/save: the text written over the file (see Save). A file that changed on disk
// is a 409 with the file's hash and the diff from it to the text, a weakening that was not
// confirmed is a 422 with the items to confirm, and nothing is written in either case.
func (s *Server) save(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text      string   `json:"text"`
		Hash      string   `json:"hash"`
		Confirmed []string `json:"confirmed"`
		Renames   []Rename `json:"renames"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if len(req.Text) > MaxFileSize {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the text is larger than %d bytes", MaxFileSize))
		return
	}
	d, err := s.saveRenamed([]byte(req.Text), req.Hash, req.Confirmed, req.Renames)
	var conflict *Conflict
	var needs *NeedsConfirmation
	switch {
	case errors.As(err, &conflict):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": conflict.Error(), "conflict": true, "hash": conflict.Hash, "diff": conflict.Diff})
	case errors.As(err, &needs):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": needs.Error(), "needs_confirmation": needs.Items})
	case err != nil:
		fail(w, http.StatusInternalServerError, err.Error())
	default:
		reply(w, d)
	}
}
