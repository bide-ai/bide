package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ModelHandler answers one model call: it returns the response the turn records. Middleware wraps
// it; the innermost handler, which the agent (or CallModel) supplies, sends each request to
// call.Model. On error the response carries at most the usage the failed request reported.
type ModelHandler func(ctx context.Context, call ModelCall) (ModelResponse, error)

// Middleware wraps a ModelHandler: the net/http-style func(Handler) Handler chain, at the
// semantic layer (it sees messages, tool calls and token usage, not bytes). Batteries live in the
// middleware package (Retry, Hedge, RateLimit, Cost). A middleware passes on the call it received,
// or a copy of it with fields changed (c := call; c.Model = backup): the call carries the agent's
// per-turn state in unexported fields, and the agent's model handler refuses a ModelCall that was
// built from scratch rather than derived from the one the middleware received (ErrConfig).
type Middleware func(next ModelHandler) ModelHandler

// ModelCall is one model call as it passes through the middleware chain. It is a value: a
// middleware changes a copy and passes that on, so what one middleware changes is seen only by
// the handlers it calls, never by its caller or by a sibling (a Hedge's other targets).
//
// Request.Messages and Request.Tools arrive at every handler slices.Clip'ped, so a handler that
// appends to them gets a new backing array and never writes into a slice another handler holds.
// The elements are shared: replace an element through a copy of the slice rather than assign into
// it.
type ModelCall struct {
	// Request is what the call asks of the model.
	Request Request
	// Model is where the agent's model handler sends the call's requests. A middleware may
	// retarget a copy of the call (Hedge sends its backups this way).
	Model Model
	// RunID is the run the call belongs to, and Turn the model turn's sequence number within it
	// (0 for the first). Both are zero for a call made through CallModel.
	RunID string
	Turn  int

	turn    *turnState      // the per-turn state the agent's model handler uses and checks
	hooks   []ModelCallHook // added through AddHook, outermost first
	attempt int             // the request's number, set on the call a hook receives
}

// AddHook returns a copy of c with h appended to its hooks, which the agent's model handler runs
// around every request it sends for the returned call and every call derived from it. Hooks are
// append-only: a middleware can add one, never remove or reorder those added outside it, and the
// agent's own spend accounting is not a hook at all, so no middleware can hide a request from the
// run's budget or Result.Spend.
func (c ModelCall) AddHook(h ModelCallHook) ModelCall {
	c.hooks = append(slices.Clip(c.hooks), h)
	return c
}

// Attempt is the number of the request a hook is observing. The turn numbers each request when it
// reaches the agent's model handler, before its Before hooks run, from one counter for the whole
// turn: 1 for the first, 2 for the next, and so on, so the requests of every Retry attempt and
// every Hedge target are numbered apart and every hook of a request, Before and After, sees the
// same number. A request a Before hook refused keeps its number although it was not sent, so the
// numbers of the requests sent can have gaps. It is 0 on a call that has not reached the model
// handler (as a middleware receives it).
func (c ModelCall) Attempt() int { return c.attempt }

// OnAnswer registers fn to run once with the call's answer: the response the whole chain returns
// to the agent, after the agent's checks, once the journal holds it as the turn's record (for
// CallModel, which has no journal, once the call returns it). It runs after every request of the
// call has had its hooks, only when the call succeeds, and not at all when the turn's record is
// not written (the write failed, or another driver of the run recorded the turn first): fn runs
// once per recorded turn. Registrations are keyed: a key registered again for the same turn (a
// middleware inside a Hedge or a Retry registers once per target or attempt) keeps the first
// registration, so fn runs once per turn wherever the middleware sits; key must be comparable, and
// a pointer the middleware allocates is the usual choice. Cost counts answers this way.
//
// It reports whether the call belongs to a turn. A call a middleware kept and passes on after its
// turn is over belongs to that turn: OnAnswer reports true and fn never runs, since the turn's
// answer is already decided. It reports false only for a call that belongs to no turn (a handler
// called directly, not through an agent or CallModel).
func (c ModelCall) OnAnswer(key any, fn func(ctx context.Context, resp ModelResponse)) bool {
	if c.turn == nil {
		return false
	}
	c.turn.onAnswer(key, fn)
	return true
}

// ModelCallHook runs around each request the agent's model handler sends: every retried attempt
// and every hedged target, wherever the middleware that added it (ModelCall.AddHook) sits in the
// chain. Middleware whose effect belongs to requests actually sent, rather than to calls through
// it, uses one: RateLimit waits for a token in Before, Cost counts spend in After.
//
// The hooks of a call run in the order they were added, so a middleware nearer the outside of the
// chain runs its Before first. Each Before runs once per request; a hook whose Before returned nil
// gets exactly one After for that request.
type ModelCallHook struct {
	// Before runs before the request is sent, with the request's context and the call as it
	// reached the model handler (Attempt set). An error fails the request without sending it:
	// the hooks after it do not run, and the hooks before it get their After with that error.
	Before func(ctx context.Context, call ModelCall) error
	// After runs once the request has ended, successfully or not.
	After func(ctx context.Context, call ModelCall, a ModelAttempt)
}

// ModelAttempt is how one request ended, as an After hook sees it.
type ModelAttempt struct {
	// Response is what the request returned. When Err is set, only its Usage is meaningful: the
	// usage the request reported before it failed, which the provider may still bill.
	Response ModelResponse
	// Discarded is usage the request's model reported as billed for responses it threw away (see
	// Finish.Discarded): zero from a live adapter, the recorded discarded spend from Replay.
	Discarded Usage
	// Err is the request's error, or the agent's rejection of its response (a reused tool-use
	// id), or the error of a Before hook that kept it from being sent.
	Err error
}

// ModelResponse is the answer to a model call: the assistant message, the usage of the request
// that produced it, and why the turn ended. Finish is never empty in a response the agent
// records: an empty reason, which a Model may send when it does not know one, counts as
// FinishStop (see Finish). The loop decides whether to run tools from the message's tool calls,
// never from Finish (an OpenAI forced tool_choice reports stop alongside tool calls).
type ModelResponse struct {
	Message   Message
	Usage     Usage
	Finish    FinishReason
	RawFinish string // the provider's own finish reason, as it sent it; empty if it sent none

	origin responseOrigin // the request that produced the response; zero for one a middleware built
}

// responseOrigin identifies the request that produced a response: its number within the turn
// (never 0), the messages and tools it sent and the model it went to, for the journal.
type responseOrigin struct {
	attempt   int
	msgs      []Message
	tools     []ToolSpec
	info      ModelInfo
	described bool
}

// CallModel sends one model call outside an agent, through mw (first = outermost) and the same
// model handler an agent uses: hooks run around every request, Messages and Tools arrive clipped,
// and the response is checked as an agent checks it (usage, finish reason, tool-use ids). There is
// no journal, no stream and no run, so RunID and Turn are zero. It returns ErrConfig when m is nil.
func CallModel(ctx context.Context, m Model, req Request, mw ...Middleware) (ModelResponse, error) {
	if m == nil {
		return ModelResponse{}, fmt.Errorf("agent: CallModel with a nil Model: %w", ErrConfig)
	}
	ts := &turnState{meter: &spendMeter{}}
	resp, err := newModelChain(mw).call(ctx, ModelCall{Request: req, Model: m}, ts)
	if err != nil {
		return ModelResponse{}, err
	}
	ts.answer(ctx, resp)
	return resp, nil
}

// journal returns what a model record journals about resp beside its message and usage: the model
// that answered, when it describes itself, and digests of the system prompt and the tool set that
// turn was sent (see Record.PromptDigest). For a response no request produced (a middleware built
// it), there is no model, and the digests are of sent, the request the agent passed to the chain.
func (resp ModelResponse) journal(sent Request) (model *ModelInfo, prompt, tools string) {
	msgs, set := sent.Messages, sent.Tools
	if o := resp.origin; o.attempt != 0 {
		msgs, set = o.msgs, o.tools
		if o.described {
			info := o.info
			model = &info
		}
	}
	return model, PromptDigest(msgs), ToolsDigest(set)
}

// PromptDigest is the hex SHA-256 digest a model record journals of the system prompt a turn was
// sent: the text of every system message in msgs, in order, each length-prefixed under a domain
// tag, so no two different prompts share a digest. It is "" when msgs holds no system message.
func PromptDigest(msgs []Message) string {
	var b []byte
	for _, m := range msgs {
		if m.Role != RoleSystem {
			continue
		}
		if b == nil {
			b = append(make([]byte, 0, 512), "bide.prompt.v1\n"...)
		}
		b = appendField(b, m.Text())
	}
	if b == nil {
		return ""
	}
	return hexDigest(b)
}

// ToolsDigest is the hex SHA-256 digest a model record journals of the tool set a turn was sent:
// each tool's name, description and argument schema, length-prefixed under a domain tag, in the
// order of their names, so the digest does not depend on the order the tools were listed in. It
// is "" for no tools.
func ToolsDigest(tools []ToolSpec) string {
	if len(tools) == 0 {
		return ""
	}
	byName := func(a, b ToolSpec) int { return strings.Compare(a.Name, b.Name) }
	if !slices.IsSortedFunc(tools, byName) {
		tools = slices.Clone(tools)
		slices.SortStableFunc(tools, byName)
	}
	b := append(make([]byte, 0, 512), "bide.tools.v1\n"...)
	for _, t := range tools {
		b = appendField(b, t.Name)
		b = appendField(b, t.Description)
		b = appendField(b, t.Input)
	}
	return hexDigest(b)
}

// appendField appends s to b as its decimal length, a colon, and s itself.
func appendField[S ~string | ~[]byte](b []byte, s S) []byte {
	b = strconv.AppendInt(b, int64(len(s)), 10)
	b = append(b, ':')
	return append(b, s...)
}

// hexDigest is the hex SHA-256 digest of b.
func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	var h [2 * sha256.Size]byte
	hex.Encode(h[:], sum[:])
	return string(h[:])
}

// usageTotals is what a run's model calls used: answer is the usage of the responses the run
// recorded, spend every request's, including requests whose responses were discarded (failed
// attempts, losing hedge targets) and model calls that failed.
type usageTotals struct {
	answer, spend Usage
}

// add counts the usage journaled in r: a model turn's response and discarded spend, a failed
// model call's spend, or a tool call's record carrying the usage of the runs it started.
func (t *usageTotals) add(r Record) {
	if r.Usage != nil {
		addUsage(&t.answer, *r.Usage)
		addUsage(&t.spend, *r.Usage)
	}
	if r.DiscardedUsage != nil {
		addUsage(&t.spend, *r.DiscardedUsage)
	}
}

// protocol:spend begin Call RequestEnds EndsAfterReturn

// spendMeter collects the usage of the model requests a run sends until the run takes it to
// record, and counts the requests in flight. A request that ends after its turn was recorded (a
// hedge loser that outlived the race) is taken with the next turn, or, at the run's end, waited
// for (wait) and recorded on its own (a lateSpendStep record).
type spendMeter struct {
	mu      sync.Mutex
	pending Usage
	active  int           // requests in flight
	idle    chan struct{} // closed when active falls to 0; nil while none is in flight
}

// begin counts a request in flight.
func (m *spendMeter) begin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == 0 {
		m.idle = make(chan struct{})
	}
	m.active++
}

// end counts a request out of flight.
func (m *spendMeter) end() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active--; m.active == 0 {
		close(m.idle)
		m.idle = nil
	}
}

// waitUntil waits until no request is in flight, ctx is done, or the deadline has passed,
// whichever is first.
func (m *spendMeter) waitUntil(ctx context.Context, deadline time.Time) {
	m.mu.Lock()
	idle := m.idle
	m.mu.Unlock()
	if idle == nil {
		return
	}
	d := time.Until(deadline)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-idle:
	case <-ctx.Done():
	case <-t.C:
	}
}

func (m *spendMeter) add(u Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	addUsage(&m.pending, u)
}

func (m *spendMeter) take() Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.pending
	m.pending = Usage{}
	return u
}

// discardedSpend is the part of spent beyond answer, the usage of the response a turn recorded,
// per field and never below zero: a response a middleware supplied without sending a request
// (a cache) may report usage no request spent.
func discardedSpend(spent, answer Usage) Usage {
	sub := func(a, b int) int { return max(a-b, 0) }
	return Usage{
		InputTokens:      sub(spent.InputTokens, answer.InputTokens),
		OutputTokens:     sub(spent.OutputTokens, answer.OutputTokens),
		CacheReadTokens:  sub(spent.CacheReadTokens, answer.CacheReadTokens),
		CacheWriteTokens: sub(spent.CacheWriteTokens, answer.CacheWriteTokens),
	}
}

// protocol:spend end

// spendStepPrefix names the records that journal a failed model call's spend: "@spend/<id>", with
// a fresh id per record (a replayed run keeps the original's).
const spendStepPrefix = "@spend/"

// protocol:spend begin FailSpend SettlePending

// recordSpend journals spent, billed usage no model record carries, as the step name: a
// StepValue carrying it as DiscardedUsage (spendStep for a model call that failed, lateSpendStep
// for requests that ended late). It is written even when ctx is cancelled, the common way a call
// fails, since the requests were billed either way. It returns the record the journal holds.
func (a *Agent) recordSpend(ctx context.Context, runID, name string, spent Usage) (Record, error) {
	rec, err := a.store.do(context.WithoutCancel(ctx), runID, name, func(context.Context) (Record, error) {
		return Record{Kind: StepValue, DiscardedUsage: &spent}, nil
	})
	if err != nil {
		return Record{}, fmt.Errorf("record spend (run %s): %w (%w)", runID, err, ErrStorage)
	}
	return rec, nil
}

// lateRequestWait bounds how long a drive waits in all, when it ends (completes, pauses, fails)
// and before it records a failed call's spend, for model requests still in flight: a hedge loser that ends
// after the winner was returned, a request a middleware left running. A request that ignores the
// cancellation it was sent for longer than this is not in the run's spend.
var lateRequestWait = 2 * time.Second

// lateSpendPrefix names the records that journal the spend of requests that ended after their
// turn was recorded, or whose turn another driver recorded: "@spend-late/<id>".
const lateSpendPrefix = "@spend-late/"

// lateSpendStep is the key of a late spend record. id is fresh for each record (newSpendID), so
// two drivers of one run never write their spend under one key, where the second would be lost.
func lateSpendStep(id string) string { return lateSpendPrefix + id }

// newSpendID is a fresh id for a spend record's key.
func newSpendID() string { return newClaimID() }

// pendingSpend is spend a drive could not journal, kept by the process for the run's next drive
// in it: a spend record whose write failed (written again under the same name, at most once), or a
// model turn whose record write reported an error and whose fate a read could not settle (turn).
type pendingSpend struct {
	name   string
	spent  Usage
	turn   bool
	built  *Record               // turn: the record the drive built, to tell whether the journal holds it
	answer func(context.Context) // turn: the turn's answer functions (ModelCall.OnAnswer)
}

// pendingKey names a run of a store, by the store's identity (storeIdentity, as claims and
// in-flight steps are shared), so every Journal over the store in this process sees it.
type pendingKey struct {
	store any
	runID string
}

// pendingSpends is the process's spend waiting for a run's next drive in it.
//
// It is bounded: past maxPendingSpends entries, whole runs are dropped, oldest first (a run taken
// and kept again may be dropped by its earlier position). An entry holds a usage, a record and the
// turn's answer functions, so the memory it holds is bounded by maxPendingSpends of them. Dropped
// spend is lost only for a run the process drives again: a process that never drives the run again
// never journals kept spend either.
var pendingSpends = struct {
	sync.Mutex
	m     map[pendingKey][]pendingSpend
	n     int          // entries held, over every run
	order []pendingKey // the held runs, in the order first kept, oldest first
}{m: map[pendingKey][]pendingSpend{}}

var maxPendingSpends = 4096 // a variable so tests can lower it

// spendKey is the key of runID's pending spend for a's store: the identity of the store beneath it
// (durableIdentity, as remembered claims are keyed), so any Journal over the store, and any
// wrapper over one, sees it. ok is false for a store with no identity, whose spend is not kept.
func (a *Agent) spendKey(runID string) (pendingKey, bool) {
	id, ok := a.store.identity()
	return pendingKey{id, runID}, ok
}

// keepSpend keeps p for runID's next drive in this process.
func (a *Agent) keepSpend(runID string, p pendingSpend) {
	k, ok := a.spendKey(runID)
	if !ok {
		return
	}
	ps := &pendingSpends
	ps.Lock()
	defer ps.Unlock()
	if _, held := ps.m[k]; !held {
		ps.order = append(ps.order, k)
	}
	ps.m[k] = append(ps.m[k], p)
	ps.n++
	for ps.n > maxPendingSpends && len(ps.order) > 0 {
		old := ps.order[0]
		ps.order = ps.order[1:]
		ps.n -= len(ps.m[old])
		delete(ps.m, old)
	}
}

// takeSpend returns and forgets runID's pending spend.
func (a *Agent) takeSpend(runID string) []pendingSpend {
	k, ok := a.spendKey(runID)
	if !ok {
		return nil
	}
	pendingSpends.Lock()
	defer pendingSpends.Unlock()
	got, held := pendingSpends.m[k]
	if !held {
		return nil
	}
	pendingSpends.n -= len(got)
	delete(pendingSpends.m, k)
	// order holds each held run once, and only held runs, so it never outgrows the entries.
	if i := slices.Index(pendingSpends.order, k); i >= 0 {
		pendingSpends.order = slices.Delete(pendingSpends.order, i, i+1)
	}
	return got
}

// settlePending journals runID's pending spend at the start of a drive, from recs, the run's
// records: a spend record is written again under its name (a write that landed after all is not
// repeated), and a turn whose record the journal holds runs its answer functions, while one it
// does not hold is journaled as a failed call's spend, since the drive will ask for the turn
// again. It returns the records it wrote; what it could not write is kept for the next drive.
func (a *Agent) settlePending(ctx context.Context, runID string, recs []Record) ([]Record, error) {
	var (
		wrote []Record
		errs  []error
	)
	for _, p := range a.takeSpend(runID) {
		if _, ok := recordNamed(recs, p.name); ok && !p.turn {
			continue // the write landed after all
		}
		if p.turn {
			rec, ok := recordNamed(recs, p.name)
			switch {
			case ok && ownRecord(rec, *p.built):
				if p.answer != nil {
					p.answer(ctx)
				}
				continue
			case ok:
				// Another driver recorded the turn: this drive's requests were billed all the same.
				p = pendingSpend{name: lateSpendStep(newSpendID()), spent: p.spent}
			default:
				p = pendingSpend{name: spendStep(newSpendID()), spent: p.spent}
			}
		}
		rec, err := a.recordSpend(ctx, runID, p.name, p.spent)
		if err != nil {
			a.keepSpend(runID, p)
			errs = append(errs, err)
			continue
		}
		wrote = append(wrote, rec)
	}
	return wrote, errors.Join(errs...)
}

// protocol:spend end

// recordNamed returns the record of recs named name.
func recordNamed(recs []Record, name string) (Record, bool) {
	for _, r := range recs {
		if r.Name == name {
			return r, true
		}
	}
	return Record{}, false
}
