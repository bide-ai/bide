// Package approvals is the intake an application serves so that approvers can decide on gated
// calls over HTTP: an http.Handler that shows a pending call from the journal, and takes an
// approver's signed decision, verifies it against the recorded call, and records it through the
// application's own journal.
//
// The application stays the only writer of its journal and the only signer of its audit log. A
// decision that arrives here is written by the same process that holds the log key (an
// audit.AuditedStore signs a head over it as it is written), so there is no window in which a
// decision sits in the journal under no signed head. The approver's authority is their signature,
// not the connection: the handler needs no authentication to be correct, because a decision that
// does not verify against the call and an enrolled key records nothing, and the gate re-verifies
// every record when the run resumes. What it adds around that is availability and confidentiality:
// strict, size-bounded requests, rate limits, a cap on concurrent work, and an authorization hook
// (WithAuth) for deployments that want mutual TLS or an identity check as well.
//
// Mount it under a prefix with http.StripPrefix:
//
//	h, err := approvals.New(journal, verifierFor,
//	    approvals.OnRecorded(func(ctx context.Context, r approvals.Recorded) {
//	        if r.Decided {
//	            _, _ = agent.Run(ctx, r.RootRunID /* ... */)
//	        }
//	    }))
//	mux.Handle("/approvals/", http.StripPrefix("/approvals", h))
//
// # Wire format
//
// GET /pending?run=<id>&call=<tool-use id> returns the call as the journal records it (Pending).
// POST /decisions takes one JSON object (Request) and returns the decision's place in the journal
// and the gate's standing (Response). Every error is {"error":{"code":...,"message":...}} with a
// stable code; a message never carries a store error.
//
// A client signs agent.ApprovalDecisionBytes over the subject it was shown (run, call, tool, and
// the arguments it displayed), so what it signs is what it saw; the handler verifies the signature
// against the call in the journal, so a client shown other arguments than the journal holds
// produces a signature that does not verify and records nothing.
package approvals

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// Request is the body of POST /decisions: one approver's signed decision. Signature is the
// approver's signature over agent.ApprovalDecisionBytes of the call they were shown, base64
// (standard alphabet, padded) as encoding/json writes a []byte. DecidedAt is when the approver
// decided, by their clock, in agent.FormatTimestamp's form; it is signed.
type Request struct {
	Run       string `json:"run"`        // the run whose journal holds the call: the pause's run ID
	Call      string `json:"call"`       // the call's tool-use ID
	Approver  string `json:"approver"`   // one of the gate's approvers, exactly
	Approved  bool   `json:"approved"`   // true to approve, false to reject
	DecidedAt string `json:"decided_at"` // agent.FormatTimestamp form
	Alg       string `json:"alg"`        // the signature scheme, e.g. "ed25519"
	Signature []byte `json:"signature"`
}

// Response is the body of a successful POST /decisions.
type Response struct {
	// Recorded is true when this request wrote the decision's record, false when the journal already
	// held it (a replay, or an identical request that won a race): the same decision again changes
	// nothing, and OnRecorded runs for the request that wrote it only.
	Recorded   bool   `json:"recorded"`
	Record     string `json:"record"`      // the journal record's name (agent.DecisionStep)
	Index      int    `json:"index"`       // its position in the run's journal (0 is the header)
	RecordedAt string `json:"recorded_at"` // when it was journaled, by the recording process's clock
	Gate       Gate   `json:"gate"`        // the gate's standing after the decision, verified
	// Counted is whether this decision is the one the gate counts for its approver. It is false for a
	// decision recorded beside an earlier valid one of the same approver (an approve and a reject
	// sent at once: both are recorded, and the first in journal order counts), and Reason says why. A
	// client that reports success must check it.
	Counted bool   `json:"counted"`
	Reason  string `json:"reason,omitempty"`
	// Receipt is the decision's receipt (audit.DecisionReceipt), built from the journal record, for
	// the approver to keep: it carries the signed decision, the call's arguments, and where the
	// journal holds the record. Record, Index, RecordedAt and LeafHash are the application's word.
	Receipt audit.DecisionReceipt `json:"receipt"`
}

// Pending is the body of GET /pending: a gated call, from the journal.
type Pending struct {
	Run            string          `json:"run"`
	Call           string          `json:"call"`
	Tool           string          `json:"tool"`
	Args           json.RawMessage `json:"args,omitempty"`      // the call's arguments, as recorded (JSON)
	ArgsText       string          `json:"args_text,omitempty"` // the arguments when they are not JSON: shown as text
	ArgsSHA256     string          `json:"args_sha256"`         // SHA-256 of the canonical arguments, lowercase hex: what a signature binds
	DecisionFormat string          `json:"decision_format"`     // agent.DecisionFormat: the format a signature must be in
	State          string          `json:"state"`               // StateWaiting, StateDecided or StateEnded
	AskedAt        string          `json:"asked_at,omitempty"`  // when the run paused at the gate, by the run's clock
	Gate           Gate            `json:"gate"`
}

// The states of a gated call (Pending.State).
const (
	// StateWaiting: the run is paused at the gate and takes decisions.
	StateWaiting = "waiting"
	// StateDecided: the gate has an outcome (a tally, or the call has a result); it takes no more.
	StateDecided = "decided"
	// StateEnded: the run ended; it takes no more.
	StateEnded = "ended"
)

// Gate is the standing of an m-of-n gate, counted as the gate counts: only decisions that verify
// under the enrolled keys, the first of each approver.
type Gate struct {
	Need      int            `json:"need"`
	Approvers []string       `json:"approvers"`
	Approved  int            `json:"approved"`
	Denied    int            `json:"denied"`
	Pending   []string       `json:"pending,omitempty"` // approvers with no counted decision
	Passed    bool           `json:"passed"`            // approved >= need
	Failed    bool           `json:"failed"`            // too few approvers left to reach need
	Decisions []GateDecision `json:"decisions,omitempty"`
}

// GateDecision is one decision record the gate read.
type GateDecision struct {
	Approver string `json:"approver"`
	Approved bool   `json:"approved"`
	Counted  bool   `json:"counted"`
	Reason   string `json:"reason,omitempty"` // why it does not count
}

// Recorded is what OnRecorded is told about a decision this handler wrote.
type Recorded struct {
	RunID     string // the run whose journal holds the call
	RootRunID string // the run to re-invoke to continue it (agent.RootRunID)
	ToolUseID string
	Approver  string
	Approved  bool
	Record    string // the journal record's name
	Index     int    // its position in the run's journal
	// Decided is true when the gate now has enough decisions to settle: it passed, or can no longer
	// pass. A hook that re-runs the run only when Decided avoids a pointless re-drive after each of
	// the first k-1 decisions.
	Decided bool
}

// Access is what an authorization hook (WithAuth) is asked to allow.
type Access struct {
	Method    string // http.MethodGet or http.MethodPost
	RunID     string
	ToolUseID string
	Approver  string // the approver a POST names; empty for a GET
}

var (
	// ErrUnauthenticated is what a WithAuth hook returns for a request with no valid credentials:
	// the handler answers 401.
	ErrUnauthenticated = errors.New("approvals: unauthenticated")
	// ErrForbidden is what a WithAuth hook returns for an authenticated request it refuses: the
	// handler answers 403. Any other error from the hook is answered the same.
	ErrForbidden = errors.New("approvals: forbidden")
)

// Defaults of the handler's bounds.
const (
	// DefaultMaxBodyBytes bounds a request body: a hybrid signature in base64 is about 4.7 KiB.
	DefaultMaxBodyBytes = 64 << 10
	// DefaultReadTimeout bounds how long a POST may take to send its body.
	DefaultReadTimeout = 10 * time.Second
	// DefaultRate and DefaultBurst are the per-client request rate and burst.
	DefaultRate  = 2.0
	DefaultBurst = 10
	// DefaultGlobalRate and DefaultGlobalBurst are the limit over all clients together.
	DefaultGlobalRate  = 50.0
	DefaultGlobalBurst = 100
	// DefaultMaxClients bounds the per-client buckets kept.
	DefaultMaxClients = 10000
	// DefaultMaxInFlight bounds the requests that read or write the journal at once, and the
	// OnRecorded hooks running at once.
	DefaultMaxInFlight = 16
	maxIDBytes         = 1024 // run and call ids
	maxApproverBytes   = 256
	maxSigBytes        = 16 << 10
)

// Option configures New.
type Option func(*Handler) error

// Handler is the intake. Create it with New; it is safe for concurrent use.
type Handler struct {
	j           *agent.Journal
	verifiers   agent.ApproverVerifierFor
	now         func() time.Time
	maxBody     int64
	readTimeout time.Duration
	auth        func(*http.Request, Access) error
	clientKey   func(*http.Request) string
	onErr       func(error)
	onRec       func(context.Context, Recorded)
	lim         *limiter
	rate        float64
	burst       int
	gRate       float64
	gBurst      int
	maxKeys     int
	sem         chan struct{} // journal work in flight
	hooks       chan struct{} // OnRecorded hooks in flight
	wg          sync.WaitGroup
	mux         *http.ServeMux
	inflight    int
}

// New returns the intake handler over j, the application's own journal: for decisions to be signed
// as they are written, j's store must be the audit.AuditedStore (or wrap it). verifierFor resolves
// an approver id to the key enrolled for it, as agent.WithApproverVerifiers does; a decision whose
// approver has no key, or whose signature does not verify under it, is refused.
func New(j *agent.Journal, verifierFor agent.ApproverVerifierFor, opts ...Option) (*Handler, error) {
	if j == nil {
		return nil, fmt.Errorf("approvals: nil journal: %w", agent.ErrConfig)
	}
	if verifierFor == nil {
		return nil, fmt.Errorf("approvals: nil approver verifiers: %w", agent.ErrConfig)
	}
	h := &Handler{j: j, verifiers: verifierFor, now: time.Now, maxBody: DefaultMaxBodyBytes, readTimeout: DefaultReadTimeout,
		rate: DefaultRate, burst: DefaultBurst, gRate: DefaultGlobalRate, gBurst: DefaultGlobalBurst,
		maxKeys: DefaultMaxClients, inflight: DefaultMaxInFlight,
		clientKey: remoteHost, onErr: func(error) {}}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o(h); err != nil {
			return nil, err
		}
	}
	h.lim = newLimiter(h.rate, h.burst, h.gRate, h.gBurst, h.maxKeys, h.now)
	h.sem = make(chan struct{}, h.inflight)
	h.hooks = make(chan struct{}, h.inflight)
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("GET /pending", h.guard(h.pending))
	h.mux.HandleFunc("POST /decisions", h.guard(h.decide))
	return h, nil
}

func configErr(format string, args ...any) error {
	return fmt.Errorf("approvals: "+format+": %w", append(args, agent.ErrConfig)...)
}

// WithClock sets the clock the rate limits run on and the decision's recorded_at is read from.
func WithClock(now func() time.Time) Option {
	return func(h *Handler) error {
		if now == nil {
			return configErr("nil clock")
		}
		h.now = now
		return nil
	}
}

// WithMaxBodyBytes bounds a request body (DefaultMaxBodyBytes by default). A larger body is 413.
func WithMaxBodyBytes(n int64) Option {
	return func(h *Handler) error {
		if n < 1 {
			return configErr("body limit must be at least 1, got %d", n)
		}
		h.maxBody = n
		return nil
	}
}

// WithReadTimeout bounds how long a POST /decisions may take to send its body (DefaultReadTimeout).
// The request holds an in-flight slot while it is read, so a stalled client costs one slot for at
// most this long. It does not replace http.Server.ReadHeaderTimeout, which bounds the headers
// before the handler runs: set both.
func WithReadTimeout(d time.Duration) Option {
	return func(h *Handler) error {
		if d <= 0 {
			return configErr("read timeout must be positive, got %v", d)
		}
		h.readTimeout = d
		return nil
	}
}

// WithAuth sets a hook that runs for every request, after the rate limit and before the journal is
// read or written, with what the request asks for. Return nil to allow it, ErrUnauthenticated for
// a request with no valid credentials (401), or ErrForbidden, or any other error, to refuse it
// (403). It is where a deployment checks a client certificate (r.TLS.PeerCertificates) or an
// identity, and ties an approver id to it (Access.Approver). The signature is the authorization of
// a decision; this hook decides who may reach the endpoint at all, and who may read a call's
// arguments, which GET /pending shows to any caller it allows.
func WithAuth(fn func(*http.Request, Access) error) Option {
	return func(h *Handler) error { h.auth = fn; return nil }
}

// WithRateLimit sets the per-client request rate (a second) and burst; DefaultRate and
// DefaultBurst by default. A rate of 0 turns the per-client limit off.
func WithRateLimit(rate float64, burst int) Option {
	return func(h *Handler) error {
		if rate < 0 || (rate > 0 && burst < 1) {
			return configErr("rate limit %v with burst %d", rate, burst)
		}
		h.rate, h.burst = rate, burst
		return nil
	}
}

// WithGlobalRateLimit sets the request rate and burst over all clients together;
// DefaultGlobalRate and DefaultGlobalBurst by default. A rate of 0 turns it off.
func WithGlobalRateLimit(rate float64, burst int) Option {
	return func(h *Handler) error {
		if rate < 0 || (rate > 0 && burst < 1) {
			return configErr("global rate limit %v with burst %d", rate, burst)
		}
		h.gRate, h.gBurst = rate, burst
		return nil
	}
}

// WithMaxClients bounds how many clients' buckets are kept (DefaultMaxClients). Past it, new
// clients share one bucket, so a flood of sources is limited as one.
func WithMaxClients(n int) Option {
	return func(h *Handler) error {
		if n < 1 {
			return configErr("max clients must be at least 1, got %d", n)
		}
		h.maxKeys = n
		return nil
	}
}

// WithClientKey names the client a request is limited as. By default it is the host of
// r.RemoteAddr; behind a reverse proxy or load balancer that is the proxy's address, so every
// client shares one bucket: set it there, to a function that reads the address the proxy vouches
// for (and only that header, which the proxy must overwrite, never pass through from the client).
func WithClientKey(fn func(*http.Request) string) Option {
	return func(h *Handler) error {
		if fn == nil {
			return configErr("nil client key function")
		}
		h.clientKey = fn
		return nil
	}
}

// WithMaxInFlight bounds the requests working on the journal at once, and the OnRecorded hooks
// running at once (DefaultMaxInFlight). A request past it is answered 503 with Retry-After.
func WithMaxInFlight(n int) Option {
	return func(h *Handler) error {
		if n < 1 {
			return configErr("max in flight must be at least 1, got %d", n)
		}
		h.inflight = n
		return nil
	}
}

// WithOnError sets the function the handler tells of what it does not show a caller: a store error,
// a refused hook. It may be called from several goroutines.
func WithOnError(fn func(error)) Option {
	return func(h *Handler) error {
		if fn == nil {
			return configErr("nil error function")
		}
		h.onErr = fn
		return nil
	}
}

// OnRecorded sets the function called, in a goroutine of its own and with a context that outlives
// the request, once for each decision this handler wrote (never for a replay or a refusal). It is
// where the application re-runs the run, or schedules it, by its own policy:
//
//	if r.Decided { _, err := agent.Run(ctx, r.RootRunID, ...) }
//
// It is an accelerator, not the guarantee: a run waiting on a decision is also picked up by
// agent.RecoverLoop, and the hook is dropped, with a report to WithOnError, when
// WithMaxInFlight hooks are already running. Wait waits for the running hooks.
func OnRecorded(fn func(context.Context, Recorded)) Option {
	return func(h *Handler) error { h.onRec = fn; return nil }
}

// Wait blocks until the OnRecorded hooks running now have returned, for a graceful shutdown:
// stop the server, then Wait.
func (h *Handler) Wait() { h.wg.Wait() }

// ServeHTTP serves GET /pending and POST /decisions.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.mux.ServeHTTP(w, r)
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// guard rate limits a request before it is read.
func (h *Handler) guard(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := h.lim.take(h.clientKey(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int((wait+time.Second-1)/time.Second))))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; retry later")
			return
		}
		next(w, r)
	}
}

// work takes a slot for work on the journal, or answers 503.
func (h *Handler) work(w http.ResponseWriter) (release func(), ok bool) {
	select {
	case h.sem <- struct{}{}:
		return func() { <-h.sem }, true
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "busy", "too many requests in progress; retry later")
		return nil, false
	}
}

// authorize runs the hook, if any, and answers a refusal.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, a Access) bool {
	if h.auth == nil {
		return true
	}
	err := h.auth(r, a)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
	default:
		if !errors.Is(err, ErrForbidden) {
			h.onErr(fmt.Errorf("approvals: authorization hook: %w", err))
		}
		writeError(w, http.StatusForbidden, "forbidden", "not allowed")
	}
	return false
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var e apiError
	e.Error.Code, e.Error.Message = code, msg
	writeJSON(w, status, e)
}

// gateRecords is a call's gate as the journal records it.
type gateRecords struct {
	recs    []agent.Record
	subject agent.ApprovalSubject
	wait    agent.ApprovalWait
}

var errNoGate = errors.New("no waiting approval gate for this call")

// load reads run's journal and finds the gate on call. It returns errNoGate for a run with no
// records, a call that is not in it, and a call with no approval-pending note: the three read the
// same, so the answer does not say which runs exist.
func (h *Handler) load(ctx context.Context, run, call string) (*gateRecords, error) {
	recs, err := h.j.History(ctx, run)
	if err != nil {
		return nil, err
	}
	_, tu, found := agent.FindToolCall(recs, call)
	if !found {
		return nil, errNoGate
	}
	g := &gateRecords{recs: recs, subject: agent.ApprovalSubject{RunID: run, ToolUseID: call, ToolName: tu.Name, Args: tu.Args}}
	name := agent.ApprovalPendingStep(call)
	for _, r := range recs {
		if r.Kind == agent.StepValue && r.Name == name {
			if err := audit.UnmarshalStrict(r.Result, &g.wait); err != nil || g.wait.ToolUseID != call {
				return nil, errNoGate
			}
			return g, nil
		}
	}
	return nil, errNoGate
}

// state says whether the gate takes decisions: the call has a result or the gate a recorded
// tally (decided), or the run ended.
func (h *Handler) state(ctx context.Context, g *gateRecords) (string, error) {
	byName := make(map[string]agent.Record, len(g.recs))
	for _, r := range g.recs {
		byName[r.Name] = r
		if (r.Kind == agent.StepToolResult || r.Kind == agent.StepSagaFail) && r.ToolUseID == g.subject.ToolUseID {
			return StateDecided, nil
		}
	}
	if _, ok := agent.RecordedTally(byName, g.subject.ToolUseID); ok {
		return StateDecided, nil
	}
	st, err := agent.Status(ctx, h.j, g.subject.RunID)
	if err != nil {
		return "", err
	}
	switch st.State {
	case agent.RunCompleted, agent.RunAborted, agent.RunCancelled:
		return StateEnded, nil
	}
	return StateWaiting, nil
}

// gate counts the gate's decisions as the gate does.
func (h *Handler) gate(g *gateRecords) Gate {
	out, _ := h.gateChecks(g)
	return out
}

// gateChecks is gate with the classification of every decision record the gate read.
func (h *Handler) gateChecks(g *gateRecords) (Gate, []agent.DecisionCheck) {
	p := agent.ApprovalPolicy{Need: g.wait.Need, Approvers: g.wait.Approvers}
	t, checks := agent.TallyApprovals(g.recs, g.subject, p, h.verifiers)
	out := Gate{Need: t.Need, Approvers: slices.Clone(t.Approvers), Approved: t.Approved, Denied: t.Denied,
		Pending: slices.Clone(t.Pending), Passed: t.Passed(), Failed: t.Unreachable()}
	for _, c := range checks {
		out.Decisions = append(out.Decisions, GateDecision{Approver: c.Approver, Approved: c.Approved, Counted: c.Counted, Reason: c.Reason})
	}
	return out, checks
}

func (h *Handler) pending(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	run, call := q.Get("run"), q.Get("call")
	if run == "" || call == "" || len(run) > maxIDBytes || len(call) > maxIDBytes || len(q) != 2 {
		writeError(w, http.StatusBadRequest, "bad_request", "give run and call, and nothing else")
		return
	}
	if !h.authorize(w, r, Access{Method: r.Method, RunID: run, ToolUseID: call}) {
		return
	}
	release, ok := h.work(w)
	if !ok {
		return
	}
	defer release()
	g, err := h.load(r.Context(), run, call)
	if err != nil {
		h.fail(w, err)
		return
	}
	st, err := h.state(r.Context(), g)
	if err != nil {
		h.fail(w, err)
		return
	}
	p := Pending{Run: run, Call: call, Tool: g.subject.ToolName, ArgsSHA256: audit.ApprovalArgsDigest(g.subject),
		DecisionFormat: agent.DecisionFormat, State: st, AskedAt: g.wait.RecordedAt, Gate: h.gate(g)}
	switch args := g.subject.Args; {
	case len(args) == 0:
		p.Args = json.RawMessage(`{}`)
	case json.Valid(args):
		p.Args = slices.Clone(args)
	default:
		p.ArgsText = string(args)
	}
	writeJSON(w, http.StatusOK, p)
}

// fail answers an error that is not a refusal of the request: the gate is not there, or the store
// failed, which is reported to WithOnError and never shown.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, errNoGate) {
		writeError(w, http.StatusNotFound, "not_found", errNoGate.Error())
		return
	}
	h.onErr(fmt.Errorf("approvals: %w", err))
	writeError(w, http.StatusInternalServerError, "storage_error", "the journal could not be read or written")
}

func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	// The in-flight slot is taken before the body is read, and the read has a deadline of its own, so
	// a client that sends headers and stalls holds one slot for at most readTimeout, not a goroutine
	// for as long as the server's timeouts allow (set http.Server.ReadHeaderTimeout too).
	release, ok := h.work(w)
	if !ok {
		return
	}
	defer release()
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(h.readTimeout)) // a writer with no deadlines: the server's own apply
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send application/json")
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", "the request body could not be read")
		}
		return
	}
	var req Request
	if err := audit.UnmarshalStrict(b, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the body is not exactly a decision: "+shorten(err.Error()))
		return
	}
	at, err := agent.ParseTimestamp(req.DecidedAt)
	switch {
	case req.Run == "" || req.Call == "" || req.Approver == "" || req.Alg == "" || len(req.Signature) == 0:
		writeError(w, http.StatusBadRequest, "bad_request", "run, call, approver, alg and signature are required")
		return
	case len(req.Run) > maxIDBytes || len(req.Call) > maxIDBytes || len(req.Approver) > maxApproverBytes || len(req.Alg) > 64 || len(req.Signature) > maxSigBytes:
		writeError(w, http.StatusBadRequest, "bad_request", "a field is too long")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", "decided_at must be of the form 2006-01-02T15:04:05.000Z")
		return
	}
	d := agent.Decision{RunID: req.Run, ToolUseID: req.Call, ApproverID: req.Approver, Approved: req.Approved, DecidedAt: at,
		Alg: agent.Alg(req.Alg), Signature: req.Signature}
	if !h.authorize(w, r, Access{Method: r.Method, RunID: req.Run, ToolUseID: req.Call, Approver: req.Approver}) {
		return
	}
	ctx := r.Context()
	g, err := h.load(ctx, d.RunID, d.ToolUseID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if g.wait.Single {
		writeError(w, http.StatusUnprocessableEntity, "single_gate", "this is a single-approver gate, which takes no signed decision; use agent.Approve")
		return
	}
	if strings.HasPrefix(d.ToolUseID, "node:") {
		writeError(w, http.StatusUnprocessableEntity, "unsupported", "the endpoint does not take decisions on plan flow node gates")
		return
	}
	if !slices.Contains(g.wait.Approvers, d.ApproverID) {
		writeError(w, http.StatusForbidden, "not_an_approver", "the approver is not one of this gate's approvers")
		return
	}
	step := agent.DecisionStep(d)
	idx := slices.IndexFunc(g.recs, func(rec agent.Record) bool { return rec.Name == step })
	wrote := false
	if idx < 0 {
		st, err := h.state(ctx, g)
		if err != nil {
			h.fail(w, err)
			return
		}
		if st != StateWaiting {
			writeError(w, http.StatusConflict, "not_waiting", "the gate takes no more decisions: "+st)
			return
		}
		// ReportWritten says whether this request's Insert stored the record: of concurrent identical
		// requests, in this process or another, exactly one does.
		err = agent.SubmitDecision(ctx, h.j, d, agent.WithDecisionCheck(h.verifiers), agent.WithClock(h.now), agent.ReportWritten(&wrote))
		switch {
		case errors.Is(err, agent.ErrAlreadyDecided):
			writeError(w, http.StatusConflict, "already_decided", "this approver already has a decision on the call that counts")
			return
		case errors.Is(err, agent.ErrInvalidApproval):
			h.onErr(fmt.Errorf("approvals: refused decision of %q on %s/%s: %w", d.ApproverID, d.RunID, d.ToolUseID, err))
			writeError(w, http.StatusUnprocessableEntity, "invalid_approval", "the decision does not verify for this call under the approver's enrolled key")
			return
		case errors.Is(err, agent.ErrConfig):
			h.onErr(fmt.Errorf("approvals: refused decision of %q on %s/%s: %w", d.ApproverID, d.RunID, d.ToolUseID, err))
			writeError(w, http.StatusUnprocessableEntity, "rejected", "the decision cannot be recorded: "+shorten(err.Error()))
			return
		case err != nil:
			h.fail(w, err)
			return
		}
		if g, err = h.load(ctx, d.RunID, d.ToolUseID); err != nil {
			h.fail(w, err)
			return
		}
		if idx = slices.IndexFunc(g.recs, func(rec agent.Record) bool { return rec.Name == step }); idx < 0 {
			h.fail(w, fmt.Errorf("decision %s recorded but not found in run %s", step, d.RunID))
			return
		}
	}
	rc, err := audit.NewDecisionReceipt(g.subject, d, g.recs[idx], idx)
	if err != nil {
		h.fail(w, err)
		return
	}
	gate, checks := h.gateChecks(g)
	resp := Response{Recorded: wrote, Record: step, Index: idx, RecordedAt: g.recs[idx].RecordedAt(), Gate: gate, Receipt: rc}
	if i := slices.IndexFunc(checks, func(c agent.DecisionCheck) bool { return c.Step == step }); i >= 0 {
		resp.Counted, resp.Reason = checks[i].Counted, checks[i].Reason
	}
	writeJSON(w, http.StatusOK, resp)
	if wrote {
		h.recorded(r.Context(), Recorded{RunID: d.RunID, RootRunID: agent.RootRunID(d.RunID), ToolUseID: d.ToolUseID, Approver: d.ApproverID,
			Approved: d.Approved, Record: step, Index: idx, Decided: resp.Gate.Passed || resp.Gate.Failed})
	}
}

// recorded starts the OnRecorded hook, if any, outside the request.
func (h *Handler) recorded(ctx context.Context, rec Recorded) {
	if h.onRec == nil {
		return
	}
	select {
	case h.hooks <- struct{}{}:
	default:
		h.onErr(fmt.Errorf("approvals: OnRecorded for %s/%s skipped: %d hooks are running; agent.RecoverLoop will pick the run up", rec.RunID, rec.ToolUseID, cap(h.hooks)))
		return
	}
	hctx := context.WithoutCancel(ctx)
	h.wg.Go(func() {
		defer func() { <-h.hooks }()
		defer func() {
			if p := recover(); p != nil {
				h.onErr(fmt.Errorf("approvals: OnRecorded panicked: %v", p))
			}
		}()
		h.onRec(hctx, rec)
	})
}

// shorten keeps an error's text to one short line for a response: the strict decoder's message
// names a field and a rule, never a store.
func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return cmp.Or(s, "invalid")
}
