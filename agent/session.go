package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session is a durable multi-turn conversation. Each Send is one full agent run (tools,
// resume, side-effect safety and all), seeded with the transcript so far — so the agent
// remembers earlier turns. The transcript is journaled turn-by-turn in the session's journal,
// so a Session reloaded (after a restart) from the same store resumes the conversation.
//
// Layering: a turn runs under its own run ID ("<id>>@turn/<n>" for Send, "<id>>@event/<key>" for
// SendOnce, the key encoded), whose durable journal handles crash resume WITHIN the turn; the
// session's journal, "<id>>@session", records which message started each Send turn and each
// completed turn's (input, answer) so the transcript can be rebuilt. A root run ID may not
// contain '>', so no run started with Run shares a journal with a session, and IsSessionRun tells
// a session's run IDs apart from every other. Recover skips them: a turn is seeded with the
// transcript before it, which only the session holds, and its answer is recorded only by the
// session, so an unfinished turn resumes when its message is sent again (the same Send, or the
// redelivered SendOnce). Intermediate tool calls stay in the
// turn's journal and are NOT carried into later turns — the conversational memory is the
// question/answer transcript, not every tool call. Each turn also journals the transcript it
// started from (as "from/<turn run>"), so a turn resumed after a crash is seeded with exactly the
// turns its earlier model calls saw, even if other messages were answered in between.
//
// A turn belongs to the message that started it: while a Send turn is unfinished, Send with a
// different message is ErrConfig rather than resuming that turn. Several handles on one session
// (a stale handle, or two workers) never lose a turn, record one twice, or answer one message
// with another's reply; a handle that finds the journal moved on reloads it, and one that sees
// another message's turn open reads the journal again before it refuses.
//
// One driver at a time: when the store implements Leaser (MemStore, store/sqlite and
// store/postgres do, found through a Journal and through wrappers, see Capability), a turn's run
// is driven under its lease, as Lease drives a run, so two workers given one message do not both
// drive its turn. The run loads its journal only once it holds the lease, and the lease is held
// until the run returns, so the drive loads every model call an earlier drive journaled and
// WithTokenBudget's bound holds across workers. A Send or SendOnce whose turn run another driver
// holds does not drive it: if the run has finished (the holder has not released the lease yet),
// its recorded answer is returned and the turn recorded, as a drive of it would; otherwise it
// returns at once with an error wrapping ErrTurnContended, having driven nothing and recorded
// no answer. That includes a second caller on the same handle while a saga turn's rollback is in
// progress (see Send). Send the same message again later, and it returns the recorded answer, or resumes
// the turn if the other driver stopped short of it. A drive whose lease is lost returns an error
// wrapping ErrLeaseLost (see Lease). Over a store with no Leaser, two drivers of one turn still
// never record it twice, but each counts only the spend it has seen (see KNOWN-LIMITATIONS).
//
// A Session is safe for concurrent use: callers sharing one handle behave as callers on separate
// handles do, and turns of different messages run in parallel.
type Session struct {
	agent *Agent
	id    string

	// mu guards the fields below. It is held while the session journal is read or written,
	// never while a turn's run is in progress.
	mu      sync.Mutex
	history []Message // alternating user / final-assistant messages, of the answered turns
	histAt  []int     // histAt[n] is len(history) after the first n recorded turns
	chain   []string  // chain[n] is the digest of the first n recorded turns
	turns   int
	keyed   map[string]turnRecord // completed SendOnce turns by key
	starts  int                   // Send turns started (start/N records)
	open    *turnStart            // the Send turn started but not yet recorded, if any
	openAt  int                   // the index n of open's start/<n> record
	// recorded holds the run IDs of the turns loaded (turn/0 up to turn/<turns-1>), so a turn
	// whose run is among them is never appended again, however far the handle has moved on.
	recorded map[string]bool

	lease recoverConfig // the turn runs' lease holder and TTL (see Agent.Session)
}

// turnRecord is the journaled shape of one completed conversation turn.
type turnRecord struct {
	Input   string   `json:"input"`
	Message *Message `json:"message,omitempty"` // the input, when it is not a user message of one text part
	Answer  Message  `json:"answer"`
	// Cancelled records a turn closed because its run was cancelled (P14 rule 16): it has no
	// answer, and it is not part of the transcript later turns are seeded with.
	Cancelled bool   `json:"cancelled,omitempty"`
	Key       string `json:"key,omitempty"`    // SendOnce's key; empty for Send
	RunID     string `json:"run_id,omitempty"` // the run that produced the answer
	Claim     string `json:"claim,omitempty"`  // random id of the writer, to tell its record from another's
}

// turnStart is the journaled start of a Send turn: which message owns turn run RunID.
type turnStart struct {
	Input   string   `json:"input"`
	Message *Message `json:"message,omitempty"` // the input, when it is not a user message of one text part
	RunID   string   `json:"run_id"`
	Claim   string   `json:"claim"`
}

// sessionInput is input as the session journal holds it: its text, and the message itself when it
// is not a user message of one text part.
func sessionInput(input Message) (string, *Message) {
	if t, ok := plainUserText(input); ok {
		return t, nil
	}
	m := input
	return input.Text(), &m
}

// inputOf is the input message a turn's records hold.
func inputOf(text string, m *Message) Message {
	if m != nil {
		return *m
	}
	return UserText(text)
}

// sameInput reports whether a turn's recorded input is input.
func sameInput(text string, m *Message, input Message) bool {
	return sameMessage(inputOf(text, m), input)
}

// turnFrom is the journaled starting point of a turn: it was seeded with the session's first
// Turns recorded turns, whose chained digest (see chainTurn) is Digest.
type turnFrom struct {
	Turns  int    `json:"turns"`
	Digest string `json:"digest"`
}

func sessionTurnStep(n int) string        { return "turn/" + strconv.Itoa(n) }
func sessionStartStep(n int) string       { return "start/" + strconv.Itoa(n) }
func sessionFromStep(runID string) string { return "from/" + runID }

// chainTurn extends the digest of the turns before tr with tr. It hashes the run ID and claim,
// which identify the exact record written, rather than its encoding, which a store may change.
func chainTurn(prev string, tr turnRecord) string {
	h := sha256.New()
	for _, f := range []string{prev, tr.RunID, tr.Claim} {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{':'})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func newClaim() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session claim: %w (%w)", err, ErrStorage)
	}
	return hex.EncodeToString(b[:]), nil
}

// protocol:sessions begin Open

// Session opens (or reopens) a multi-turn conversation with the given id, rebuilding the
// transcript from the store so a restarted process continues where it left off.
//
// opts set the lease each turn's run is driven under (WithLeaseHolder, WithLeaseTTL; see
// Lease): a non-positive TTL is ErrConfig. They have no effect over a store with no Leaser.
//
// The id must not contain '>'. The session journals under "<id>>@session" and runs its turns
// under "<id>>@turn/<n>" and "<id>>@event/<encoded key>" (and their sub-agents under
// SubRunID(<turn run>, <call>)): everything up to the first '>' is the session id, so two
// sessions never share a run, and a root run ID may not contain '>', so no root run shares one
// with a session either.
func (a *Agent) Session(ctx context.Context, id string, opts ...LeaseOption) (*Session, error) {
	if id == "" {
		return nil, fmt.Errorf("Session: empty id: %w", ErrConfig)
	}
	if strings.Contains(id, subRunSep) {
		// Everything up to the first '>' of a session's run IDs is its id; an id with one could
		// name another session's run, or a sub-agent's (see SubRunID).
		return nil, fmt.Errorf("Session: id %q contains %q, which the engine reserves for the run IDs of sub-agents and session turns: %w", id, subRunSep, ErrConfig)
	}
	cfg, err := leaseConfig("Session", opts, LeaseOption.applyLease)
	if err != nil {
		return nil, err
	}
	s := &Session{agent: a, id: id, lease: cfg}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// reload rebuilds the session's state from its journal. The caller holds s.mu, or has not
// shared s yet.
func (s *Session) reload(ctx context.Context) error {
	recs, err := s.agent.store.History(ctx, sessionJournalID(s.id))
	if err != nil {
		return fmt.Errorf("session %s: load transcript: %w (%w)", s.id, err, ErrStorage)
	}
	s.history, s.histAt, s.chain, s.turns, s.keyed, s.starts, s.open = nil, []int{0}, []string{""}, 0, map[string]turnRecord{}, 0, nil
	byName := make(map[string]Record, len(recs))
	for _, r := range recs {
		if r.Kind == StepValue {
			byName[r.Name] = r
		}
	}
	finished := map[string]bool{} // run IDs whose turn is recorded
	s.recorded = finished
	for ; ; s.turns++ {
		r, ok := byName[sessionTurnStep(s.turns)]
		if !ok {
			break
		}
		var tr turnRecord
		if err := json.Unmarshal(r.Result, &tr); err != nil {
			return fmt.Errorf("session %s: decode turn %d: %w (%w)", s.id, s.turns, err, ErrProtocol)
		}
		if !tr.Cancelled {
			s.history = append(s.history, inputOf(tr.Input, tr.Message), tr.Answer)
		}
		s.histAt = append(s.histAt, len(s.history))
		s.chain = append(s.chain, chainTurn(s.chain[s.turns], tr))
		if tr.Key != "" {
			s.keyed[tr.Key] = tr
		}
		finished[tr.RunID] = true
	}
	for ; ; s.starts++ {
		r, ok := byName[sessionStartStep(s.starts)]
		if !ok {
			break
		}
		var st turnStart
		if err := json.Unmarshal(r.Result, &st); err != nil {
			return fmt.Errorf("session %s: decode turn start %d: %w (%w)", s.id, s.starts, err, ErrProtocol)
		}
		if !finished[st.RunID] && s.open == nil {
			s.open, s.openAt = &st, s.starts
		}
	}
	return nil
}

// protocol:sessions end

// Send runs one conversation turn: the agent answers `input` with the full prior
// transcript in context, and the turn is journaled. Returns the assistant's answer.
//
// If the turn pauses (a tool needs approval or Interrupt) or fails, Send returns that error
// (*ApprovalPending / *InterruptPending / ...) and does NOT advance the transcript; resolve it
// (Approve / AnswerInterrupt) and call Send again with the SAME input to resume that turn. Until then,
// Send with a different input is ErrConfig: the open turn belongs to its message.
//
// A turn whose run was cancelled (see Cancel) is closed: Send of its message (the same text)
// returns ErrRunCancelled, and the next Send of another message records the turn closed (with no
// answer, and outside the transcript) and runs its own turn. Cancel of a saga turn's run writes
// only its rollback request: the next Send of another message drives that rollback and then
// closes the turn. Over a store with a Leaser the rollback is driven under the turn's lease and
// without holding the handle's mutex, so the call gets ErrTurnContended while another driver holds
// the turn, and while the rollback is in progress any other caller gets ErrTurnContended: another
// worker, or a second caller on this same handle. Over a store with no Leaser the handle's mutex
// is held across the rollback, and a second caller on the handle waits for it.
//
// Deprecated: transitional; renamed by the 1.0 rewrite. Use SendMessage, which becomes Send.
func (s *Session) Send(ctx context.Context, input string) (Message, error) {
	msg, _, err := s.send(ctx, UserText(input), nil)
	return msg, err
}

// SendMessage runs one conversation turn as Send does, for input (which may carry images) under
// opts, the turn run's options (journaled in its run:start, as RunMessage journals them), and
// returns the turn's Result: non-nil whatever the error once the turn has its run (an invalid
// option, or a refusal to start the turn, such as another message's open turn, has none).
//
// Deprecated: transitional; renamed by the 1.0 rewrite. SendMessage becomes Send.
func (s *Session) SendMessage(ctx context.Context, input Message, opts ...RunOption) (*Result, error) {
	_, res, err := s.send(ctx, input, opts)
	return res, err
}

// send is Send's and SendMessage's body.
func (s *Session) send(ctx context.Context, input Message, opts []RunOption) (Message, *Result, error) {
	t0 := time.Now()
	var cfg runConfig
	if err := applyOptions("run", &cfg, opts, RunOption.applyRun); err != nil {
		return Message{}, nil, err
	}
	start, n, err := s.startTurn(ctx, input)
	if err != nil {
		return Message{}, nil, err
	}
	msg, tot, turns, err := s.runTurn(ctx, start.RunID, &SessionRef{ID: s.id, Turn: &n}, input, cfg)
	return s.turnResult(t0, start.RunID, msg, tot, turns, err)
}

// turnResult is the Result of a turn's drive.
func (s *Session) turnResult(t0 time.Time, runID string, msg Message, tot usageTotals, turns int, err error) (Message, *Result, error) {
	res := &Result{RunID: runID, Usage: tot.answer, Spend: tot.spend, Turns: turns, Duration: time.Since(t0)}
	if err == nil {
		res.Message = msg
	}
	return msg, res, err
}

// protocol:sessions begin SCheck SDo

// startTurn returns the open Send turn for input, or claims a new one, and its index. A claim lost
// to another handle reloads the journal and tries once more. An open turn of another message is
// refused only once the journal has been read again (this handle's view of it may be stale: its own
// Send of that message failed or paused, and another handle has since finished the turn), and only
// if its run was not cancelled: a cancelled turn is recorded closed first (P14 rule 16).
func (s *Session) startTurn(ctx context.Context, input Message) (turnStart, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reloaded := false
	rolledBack := "" // the turn run whose rollback this call drove: it is driven at most once
	for attempt := 0; ; attempt++ {
		if s.open != nil && !sameInput(s.open.Input, s.open.Message, input) && !reloaded {
			if err := s.reload(ctx); err != nil {
				return turnStart{}, 0, err
			}
			reloaded = true
		}
		if s.open != nil && !sameInput(s.open.Input, s.open.Message, input) {
			closed, requested, err := s.closeIfCancelled(ctx)
			if err != nil {
				return turnStart{}, 0, err
			}
			if closed {
				attempt--
				continue
			}
			if requested != "" && requested != rolledBack {
				// The saga turn's rollback, without s.mu; the journal is read again after it,
				// since another caller may have moved the session on meanwhile.
				if err := s.rollbackTurn(ctx, requested); err != nil {
					return turnStart{}, 0, err
				}
				rolledBack = requested
				if err := s.reload(ctx); err != nil {
					return turnStart{}, 0, err
				}
				attempt--
				continue
			}
		}
		if s.open != nil {
			if !sameInput(s.open.Input, s.open.Message, input) {
				return turnStart{}, 0, fmt.Errorf("session %s: a turn for %q is still open; send that message again to finish it: %w", s.id, s.open.Input, ErrConfig)
			}
			return *s.open, s.openAt, nil
		}
		claim, err := newClaim()
		if err != nil {
			return turnStart{}, 0, err
		}
		n := s.starts
		text, msg := sessionInput(input)
		st := turnStart{Input: text, Message: msg, RunID: sessionTurnRunID(s.id, n), Claim: claim}
		b, err := marshalJournal(st)
		if err != nil {
			return turnStart{}, 0, fmt.Errorf("session %s: encode turn start: %w (%w)", s.id, err, ErrConfig)
		}
		got, err := s.agent.store.Do(ctx, sessionJournalID(s.id), sessionStartStep(n), func(context.Context) (Record, error) {
			return Record{Kind: StepValue, Result: b}, nil
		})
		if err != nil {
			return turnStart{}, 0, fmt.Errorf("session %s: record turn start %d: %w (%w)", s.id, n, err, ErrStorage)
		}
		var won turnStart
		if err := json.Unmarshal(got.Result, &won); err != nil {
			return turnStart{}, 0, fmt.Errorf("session %s: decode turn start %d: %w (%w)", s.id, n, err, ErrProtocol)
		}
		if won.Claim == claim {
			s.starts++
			s.open, s.openAt = &st, n
			return st, n, nil
		}
		// Another handle started turn n first: this handle is stale. Catch up and try again.
		if attempt > 0 {
			return turnStart{}, 0, fmt.Errorf("session %s: another writer is sending on this session: %w", s.id, ErrConfig)
		}
		if err := s.reload(ctx); err != nil {
			return turnStart{}, 0, err
		}
		reloaded = true
	}
}

// closeIfCancelled records the open Send turn closed if its run was cancelled (its first end
// marker is run:cancelled), and reloads: rule 16 of the P14 contract (model 12's S3). A saga turn's
// Cancel writes only a rollback request, which the turn's next drive acts on: the session owns its
// turns, so it drives that rollback itself, and closes the turn once the rollback has written
// run:cancelled. closeIfCancelled does not drive it: it returns the turn run's ID as requested,
// and startTurn drives the rollback without s.mu (rollbackTurn) and then checks again. The caller
// holds s.mu, and s.open is set.
func (s *Session) closeIfCancelled(ctx context.Context) (closed bool, requested string, _ error) {
	runID := s.open.RunID
	cancelled, err := cancelledFirst(ctx, s.agent.store, runID)
	if err != nil || !cancelled {
		if err == nil {
			var ok bool
			if _, ok, err = lookup(ctx, s.agent.store, runID, runCancelRequestedStep); ok {
				requested = runID
			}
		}
		return false, requested, err
	}
	claim, err := newClaim()
	if err != nil {
		return false, "", err
	}
	rec := turnRecord{Input: s.open.Input, Message: s.open.Message, RunID: runID, Claim: claim, Cancelled: true}
	if err := s.appendTurn(ctx, rec); err != nil {
		return false, "", err
	}
	return true, "", s.reload(ctx)
}

// protocol:sessions end

// protocol:sessions begin SRb

// rollbackTurn drives the rollback a Cancel of the saga turn run runID requested, under the turn
// run's lease (as every drive of a turn is). Over a store with a Leaser it runs without s.mu: a
// rollback runs compensators, which may take as long as any tool call, and the lease keeps any
// other driver, another caller on this handle included, off the turn run: such a caller gets
// ErrTurnContended until the rollback is over. Over a store with no Leaser nothing else would keep
// two callers on this handle from driving the same rollback at once, so s.mu stays held across
// the drive, and another caller on the handle waits for it. The rollback needs no transcript: the
// drive's open finds the request before any model call, and the saga path compensates from the
// turn run's own journal. A rollback that stopped (a failed compensator, an unknown outcome)
// returns its error, and the turn stays open; one that found the turn's answer recorded before the
// request returns nil, and the turn is not closed. The caller holds s.mu; over a store with a
// Leaser it is released for the drive and held again when rollbackTurn returns.
func (s *Session) rollbackTurn(ctx context.Context, runID string) error {
	if _, leased := capabilityOf[Leaser](s.agent.store); leased {
		s.mu.Unlock()
		defer s.mu.Lock()
	}
	d := &driveSpec{resume: true, kind: RunKindSessionTurn, cfg: runConfig{saga: true}}
	if _, _, _, err := s.driveRun(ctx, runID, d); err != nil && !cancelledEnd(err) {
		return err
	}
	return nil
}

// protocol:sessions end

// protocol:sessions begin KLook

// SendOnce runs one conversation turn for an inbound message identified by key (an event or
// message id), at most once per key. A key whose turn already completed returns that turn's
// answer without running anything, so a redelivered message never opens a second turn, even
// if the process died after the turn was recorded and before the caller replied. A key whose
// turn was interrupted resumes that same turn: it runs under its own journal,
// "<session id>>@event/<key>" (the key encoded, so any key is allowed), so a different message
// arriving in between gets its own turn.
// Reusing a key with a different input is ErrConfig, whether the key's turn has finished or is
// still open: the turn's run records the message it answers (see RunStart).
//
// Deprecated: transitional; renamed by the 1.0 rewrite. Use SendMessageOnce, which becomes
// SendOnce.
func (s *Session) SendOnce(ctx context.Context, key, input string) (Message, error) {
	msg, _, err := s.sendOnce(ctx, key, UserText(input), nil)
	return msg, err
}

// SendMessageOnce runs one conversation turn for the message key as SendOnce does, for input under
// opts, and returns the turn's Result.
//
// Deprecated: transitional; renamed by the 1.0 rewrite. SendMessageOnce becomes SendOnce.
func (s *Session) SendMessageOnce(ctx context.Context, key string, input Message, opts ...RunOption) (*Result, error) {
	_, res, err := s.sendOnce(ctx, key, input, opts)
	return res, err
}

// sendOnce is SendOnce's and SendMessageOnce's body.
func (s *Session) sendOnce(ctx context.Context, key string, input Message, opts []RunOption) (Message, *Result, error) {
	t0 := time.Now()
	if key == "" {
		return Message{}, nil, fmt.Errorf("session %s: SendOnce: empty key: %w", s.id, ErrConfig)
	}
	var cfg runConfig
	if err := applyOptions("run", &cfg, opts, RunOption.applyRun); err != nil {
		return Message{}, nil, err
	}
	runID := sessionEventRunID(s.id, key)
	tr, ok, err := s.keyedTurn(ctx, key)
	if err != nil {
		return Message{}, &Result{RunID: runID, Duration: time.Since(t0)}, err
	}
	if ok {
		if !sameInput(tr.Input, tr.Message, input) {
			return Message{}, &Result{RunID: runID, Duration: time.Since(t0)}, fmt.Errorf("session %s: key %q was already used for a different message: %w", s.id, key, ErrConfig)
		}
		return s.turnResult(t0, runID, tr.Answer, usageTotals{}, 0, nil)
	}
	msg, tot, turns, err := s.runTurn(ctx, runID, &SessionRef{ID: s.id, Key: key}, input, cfg)
	return s.turnResult(t0, runID, msg, tot, turns, err)
}

// keyedTurn returns the completed SendOnce turn for key, reloading the journal first if this
// handle has not seen one: another handle may have answered it.
func (s *Session) keyedTurn(ctx context.Context, key string) (turnRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keyed[key]; !ok {
		if err := s.reload(ctx); err != nil {
			return turnRecord{}, false, err
		}
	}
	tr, ok := s.keyed[key]
	return tr, ok, nil
}

// protocol:sessions end

// protocol:sessions begin DLoad DCall DDone AReload

// runTurn drives the turn's run (see driveRun) and appends the completed turn to the transcript.
func (s *Session) runTurn(ctx context.Context, runID string, ref *SessionRef, input Message, cfg runConfig) (Message, usageTotals, int, error) {
	seed, err := s.turnSeed(ctx, runID)
	if err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	d := &driveSpec{input: &input, seed: seed, kind: RunKindSessionTurn, session: ref, cfg: cfg, strictSaga: !cfg.saga}
	answer, tot, turns, err := s.driveRun(ctx, runID, d)
	if err != nil {
		return answer, tot, turns, err // pause/error: transcript unadvanced; retry same input to resume
	}

	claim, err := newClaim()
	if err != nil {
		return answer, tot, turns, err
	}
	text, msg := sessionInput(input)
	rec := turnRecord{Input: text, Message: msg, Answer: answer, Key: ref.Key, RunID: runID, Claim: claim}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendTurn(ctx, rec); err != nil {
		return answer, tot, turns, err
	}
	return answer, tot, turns, s.reload(ctx)
}

// driveRun drives the turn's run (d) under its lease when the store has a Leaser (see Session),
// so the run loads its journal, and with it every model call an earlier holder made, only once it
// holds the lease. A run another holder leases is not driven: a finished one needs no lease, and
// its end is returned as a drive of it would return it (its first end marker: a completed run's
// answer, to a drive with the input it answered, or ErrRunCancelled); any other is an error
// wrapping ErrTurnContended.
func (s *Session) driveRun(ctx context.Context, runID string, d *driveSpec) (Message, usageTotals, int, error) {
	var (
		answer Message
		tot    usageTotals
		turns  int
	)
	driven, err := leaseRun(ctx, s.agent.store, runID, func(ctx context.Context) error {
		var err error
		answer, tot, turns, err = s.agent.drive(withSessionRun(ctx, runID), runID, d)
		return err
	}, s.lease)
	if err != nil || driven {
		return answer, tot, turns, err
	}
	recs, err := s.agent.store.History(ctx, runID)
	if err != nil {
		return Message{}, usageTotals{}, 0, storageErr("load history "+runID, err)
	}
	if end, ended := firstEnd(recs); ended {
		if err := checkFinishedStart(runID, recs, d.runKind(), nil); err != nil {
			return Message{}, usageTotals{}, 0, err
		}
		if end.name != runCompleteStep {
			return Message{}, journalTotals(recs), 0, endedErr(runID, end)
		}
		if err := checkFinishedStart(runID, recs, d.runKind(), d.input); err != nil {
			return Message{}, usageTotals{}, 0, err
		}
		if final, ok := completedAnswer(recs); ok {
			return final, journalTotals(recs), 0, nil
		}
	}
	return Message{}, usageTotals{}, 0, fmt.Errorf("session %s: turn run %s is driven by another holder; send the message again later: %w", s.id, runID, ErrTurnContended)
}

// protocol:sessions end

// protocol:sessions begin Seed FDo FReload

// turnSeed returns the transcript the turn run runID is seeded with. The first time the turn runs,
// it is the transcript this handle holds, and that starting point is journaled before the run
// makes any model call; every later attempt, on any handle, is seeded from the journaled one.
// A turn begun by a version that journaled no starting point is seeded, as it was then, from the
// transcript this handle holds when it resumes.
func (s *Session) turnSeed(ctx context.Context, runID string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := marshalJournal(turnFrom{Turns: s.turns, Digest: s.chain[s.turns]})
	if err != nil {
		return nil, fmt.Errorf("session %s: encode turn start point: %w (%w)", s.id, err, ErrConfig)
	}
	name := sessionFromStep(runID)
	got, err := s.agent.store.Do(ctx, sessionJournalID(s.id), name, func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: b}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("session %s: record %s: %w (%w)", s.id, name, err, ErrStorage)
	}
	var from turnFrom
	if err := json.Unmarshal(got.Result, &from); err != nil {
		return nil, fmt.Errorf("session %s: decode %s: %w (%w)", s.id, name, err, ErrProtocol)
	}
	if from.Turns > s.turns { // another handle started the turn having seen more of the journal
		if err := s.reload(ctx); err != nil {
			return nil, err
		}
	}
	if from.Turns < 0 || from.Turns > s.turns || s.chain[from.Turns] != from.Digest {
		return nil, fmt.Errorf("session %s: %s names %d turns the journal does not hold: %w", s.id, name, from.Turns, ErrProtocol)
	}
	seed := make([]Message, 0, s.histAt[from.Turns]+1)
	return append(seed, s.history[:s.histAt[from.Turns]]...), nil
}

// protocol:sessions end

// protocol:sessions begin ADo

// appendTurn records rec at the next free turn index. A slot another handle filled first is
// skipped rather than overwritten, so no turn is lost; a slot that already holds this run's
// turn (another handle, or an earlier attempt, recorded it) ends the append, and so does a run
// among the turns this handle has loaded, which the scan from s.turns would not see (another
// caller on this handle recorded it and reloaded), so no turn is recorded twice. Every handle
// for one message drives the same run ID, which makes those checks sufficient. The caller holds
// s.mu.
func (s *Session) appendTurn(ctx context.Context, rec turnRecord) error {
	if s.recorded[rec.RunID] {
		return nil // recorded at a slot below s.turns
	}
	b, err := marshalJournal(rec)
	if err != nil {
		return fmt.Errorf("session %s: encode turn: %w (%w)", s.id, err, ErrConfig)
	}
	for n := s.turns; ; n++ {
		got, err := s.agent.store.Do(ctx, sessionJournalID(s.id), sessionTurnStep(n), func(context.Context) (Record, error) {
			return Record{Kind: StepValue, Result: b}, nil
		})
		if err != nil {
			return fmt.Errorf("session %s: record turn %d: %w (%w)", s.id, n, err, ErrStorage)
		}
		var at turnRecord
		if err := json.Unmarshal(got.Result, &at); err != nil {
			return fmt.Errorf("session %s: decode turn %d: %w (%w)", s.id, n, err, ErrProtocol)
		}
		if at.Claim == rec.Claim {
			return nil
		}
		if at.RunID == rec.RunID {
			return nil // this run's turn was already recorded (a retry after a lost reply)
		}
	}
}

// protocol:sessions end

// History returns a copy of the conversation transcript so far (alternating user and
// final-assistant messages).
func (s *Session) History() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.history))
	copy(out, s.history)
	return out
}

// Turns returns the number of completed turns.
func (s *Session) Turns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turns
}
