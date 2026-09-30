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
)

// Session is a durable multi-turn conversation. Each Send is one full agent run (tools,
// resume, side-effect safety and all), seeded with the transcript so far — so the agent
// remembers earlier turns. The transcript is journaled turn-by-turn under the session id,
// so a Session reloaded (after a restart) from the same store resumes the conversation.
//
// Layering: a turn runs under its own run ID ("<id>/tN" for Send, "<id>/e/<key>" for
// SendOnce), whose durable journal handles crash resume WITHIN the turn; the session-level
// journal under "<id>" records which message started each Send turn and each completed turn's
// (input, answer) so the transcript can be rebuilt. Intermediate tool calls stay in the
// turn's journal and are NOT carried into later turns — the conversational memory is the
// question/answer transcript, not every tool call. Each turn also journals the transcript it
// started from (as "from/<turn run>"), so a turn resumed after a crash is seeded with exactly the
// turns its earlier model calls saw, even if other messages were answered in between.
//
// A turn belongs to the message that started it: while a Send turn is unfinished, Send with a
// different message is ErrConfig rather than resuming that turn. Several handles on one session
// (a stale handle, or two workers) never lose a turn or answer one message with another's
// reply; a handle that finds the journal moved on reloads it.
//
// A Session is safe for concurrent use: callers sharing one handle behave as callers on separate
// handles do, and their turns run in parallel.
type Session struct {
	agent *Agent
	id    string

	// mu guards the fields below. It is held while the session journal is read or written,
	// never while a turn's run is in progress.
	mu      sync.Mutex
	history []Message // alternating user / final-assistant messages
	chain   []string  // chain[n] is the digest of the first n recorded turns
	turns   int
	keyed   map[string]turnRecord // completed SendOnce turns by key
	starts  int                   // Send turns started (start/N records)
	open    *turnStart            // the Send turn started but not yet recorded, if any
}

// turnRecord is the journaled shape of one completed conversation turn.
type turnRecord struct {
	Input  string  `json:"input"`
	Answer Message `json:"answer"`
	Key    string  `json:"key,omitempty"`    // SendOnce's key; empty for Send
	RunID  string  `json:"run_id,omitempty"` // the run that produced the answer
	Claim  string  `json:"claim,omitempty"`  // random id of the writer, to tell its record from another's
}

// turnStart is the journaled start of a Send turn: which message owns turn run RunID.
type turnStart struct {
	Input string `json:"input"`
	RunID string `json:"run_id"`
	Claim string `json:"claim"`
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

// Session opens (or reopens) a multi-turn conversation with the given id, rebuilding the
// transcript from the store so a restarted process continues where it left off.
//
// The id must not contain '/'. The session journals under the id itself and runs its turns
// under "<id>/t<n>" and "<id>/e/<key>" (and their sub-agents under SubRunID of the turn run and
// the call), so an id with a '/' could name another session's turn: session "c/e" and session "c" answering key "t0"
// would share run "c/e/t0", and one would be handed the other's reply. Those names belong to the
// session; do not pass them to Run.
func (a *Agent) Session(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, fmt.Errorf("Session: empty id: %w", ErrConfig)
	}
	if strings.ContainsRune(id, '/') {
		return nil, fmt.Errorf("Session: id %q contains '/': %w", id, ErrConfig)
	}
	if IsSubRun(id) {
		// The session's turns run under its id; one with the sub-run separator could name a
		// sub-agent's run (see SubRunID).
		return nil, fmt.Errorf("Session: id %q contains %q, which separates a sub-agent's run from its parent's: %w", id, subRunSep, ErrConfig)
	}
	s := &Session{agent: a, id: id}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// reload rebuilds the session's state from its journal. The caller holds s.mu, or has not
// shared s yet.
func (s *Session) reload(ctx context.Context) error {
	recs, err := s.agent.store.History(ctx, s.id)
	if err != nil {
		return fmt.Errorf("session %s: load transcript: %w (%w)", s.id, err, ErrStorage)
	}
	s.history, s.chain, s.turns, s.keyed, s.starts, s.open = nil, []string{""}, 0, map[string]turnRecord{}, 0, nil
	byName := make(map[string]Record, len(recs))
	for _, r := range recs {
		if r.Kind == StepValue {
			byName[r.Name] = r
		}
	}
	finished := map[string]bool{} // run IDs whose turn is recorded
	for ; ; s.turns++ {
		r, ok := byName[sessionTurnStep(s.turns)]
		if !ok {
			break
		}
		var tr turnRecord
		if err := json.Unmarshal(r.Result, &tr); err != nil {
			return fmt.Errorf("session %s: decode turn %d: %w (%w)", s.id, s.turns, err, ErrProtocol)
		}
		s.history = append(s.history, UserText(tr.Input), tr.Answer)
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
			s.open = &st
		}
	}
	return nil
}

// Send runs one conversation turn: the agent answers `input` with the full prior
// transcript in context, and the turn is journaled. Returns the assistant's answer.
//
// If the turn pauses (a tool needs approval or Interrupt) or fails, Send returns that error
// (*PendingApproval / *Interrupted / ...) and does NOT advance the transcript; resolve it
// (Approve / Resume) and call Send again with the SAME input to resume that turn. Until then,
// Send with a different input is ErrConfig: the open turn belongs to its message.
func (s *Session) Send(ctx context.Context, input string) (Message, error) {
	start, err := s.startTurn(ctx, input)
	if err != nil {
		return Message{}, err
	}
	return s.runTurn(ctx, start.RunID, "", input)
}

// startTurn returns the open Send turn for input, or claims a new one. A claim lost to another
// handle reloads the journal and tries once more.
func (s *Session) startTurn(ctx context.Context, input string) (turnStart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if s.open != nil {
			if s.open.Input != input {
				return turnStart{}, fmt.Errorf("session %s: a turn for %q is still open; send that message again to finish it: %w", s.id, s.open.Input, ErrConfig)
			}
			return *s.open, nil
		}
		claim, err := newClaim()
		if err != nil {
			return turnStart{}, err
		}
		n := s.starts
		st := turnStart{Input: input, RunID: s.id + "/t" + strconv.Itoa(n), Claim: claim}
		b, err := json.Marshal(st)
		if err != nil {
			return turnStart{}, fmt.Errorf("session %s: encode turn start: %w (%w)", s.id, err, ErrConfig)
		}
		got, err := s.agent.store.Do(ctx, s.id, sessionStartStep(n), func(context.Context) (Record, error) {
			return Record{Kind: StepValue, Result: b}, nil
		})
		if err != nil {
			return turnStart{}, fmt.Errorf("session %s: record turn start %d: %w (%w)", s.id, n, err, ErrStorage)
		}
		var won turnStart
		if err := json.Unmarshal(got.Result, &won); err != nil {
			return turnStart{}, fmt.Errorf("session %s: decode turn start %d: %w (%w)", s.id, n, err, ErrProtocol)
		}
		if won.Claim == claim {
			s.starts++
			s.open = &st
			return st, nil
		}
		// Another handle started turn n first: this handle is stale. Catch up and try again.
		if attempt > 0 {
			return turnStart{}, fmt.Errorf("session %s: another writer is sending on this session: %w", s.id, ErrConfig)
		}
		if err := s.reload(ctx); err != nil {
			return turnStart{}, err
		}
	}
}

// SendOnce runs one conversation turn for an inbound message identified by key (an event or
// message id), at most once per key. A key whose turn already completed returns that turn's
// answer without running anything, so a redelivered message never opens a second turn, even
// if the process died after the turn was recorded and before the caller replied. A key whose
// turn was interrupted resumes that same turn: it runs under its own journal,
// "<session id>/e/<key>", so a different message arriving in between gets its own turn.
// Reusing a key with a different input is ErrConfig, whether the key's turn has finished or is
// still open: the turn's run records the message it answers (see RunStart).
func (s *Session) SendOnce(ctx context.Context, key, input string) (Message, error) {
	if key == "" {
		return Message{}, fmt.Errorf("session %s: SendOnce: empty key: %w", s.id, ErrConfig)
	}
	if strings.ContainsRune(key, '/') {
		return Message{}, fmt.Errorf("session %s: SendOnce: key %q contains '/': %w", s.id, key, ErrConfig)
	}
	tr, ok, err := s.keyedTurn(ctx, key)
	if err != nil {
		return Message{}, err
	}
	if ok {
		if tr.Input != input {
			return Message{}, fmt.Errorf("session %s: key %q was already used for a different message: %w", s.id, key, ErrConfig)
		}
		return tr.Answer, nil
	}
	return s.runTurn(ctx, s.id+"/e/"+key, key, input)
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

// runTurn drives the turn's run and appends the completed turn to the transcript.
func (s *Session) runTurn(ctx context.Context, runID, key, input string) (Message, error) {
	seed, err := s.turnSeed(ctx, runID)
	if err != nil {
		return Message{}, err
	}
	seed = append(seed, UserText(input))

	answer, _, _, err := s.agent.run(ctx, runID, seed, false, nil)
	if err != nil {
		return answer, err // pause/error: transcript unadvanced; retry same input to resume
	}

	claim, err := newClaim()
	if err != nil {
		return answer, err
	}
	rec := turnRecord{Input: input, Answer: answer, Key: key, RunID: runID, Claim: claim}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendTurn(ctx, rec); err != nil {
		return answer, err
	}
	return answer, s.reload(ctx)
}

// turnSeed returns the transcript the turn run runID is seeded with. The first time the turn runs,
// it is the transcript this handle holds, and that starting point is journaled before the run
// makes any model call; every later attempt, on any handle, is seeded from the journaled one.
// A turn begun by a version that journaled no starting point is seeded, as it was then, from the
// transcript this handle holds when it resumes.
func (s *Session) turnSeed(ctx context.Context, runID string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(turnFrom{Turns: s.turns, Digest: s.chain[s.turns]})
	if err != nil {
		return nil, fmt.Errorf("session %s: encode turn start point: %w (%w)", s.id, err, ErrConfig)
	}
	name := sessionFromStep(runID)
	got, err := s.agent.store.Do(ctx, s.id, name, func(context.Context) (Record, error) {
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
	seed := make([]Message, 0, 2*from.Turns+1)
	return append(seed, s.history[:2*from.Turns]...), nil
}

// appendTurn records rec at the next free turn index. A slot another handle filled first is
// skipped rather than overwritten, so no turn is lost; a slot that already holds this run's
// turn (another handle, or an earlier attempt, recorded it) ends the append, so no turn is
// recorded twice. Every handle for one message drives the same run ID, which makes that check
// sufficient. The caller holds s.mu.
func (s *Session) appendTurn(ctx context.Context, rec turnRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("session %s: encode turn: %w (%w)", s.id, err, ErrConfig)
	}
	for n := s.turns; ; n++ {
		got, err := s.agent.store.Do(ctx, s.id, sessionTurnStep(n), func(context.Context) (Record, error) {
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
