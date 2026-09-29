package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// Session is a durable multi-turn conversation. Each Send is one full agent run (tools,
// resume, side-effect safety and all), seeded with the transcript so far — so the agent
// remembers earlier turns. The transcript is journaled turn-by-turn under the session id,
// so a Session reloaded (after a restart) from the same store resumes the conversation.
//
// Layering: turn N runs under runID "<id>/tN" (its own durable journal handles crash
// resume WITHIN the turn); the session-level journal under "<id>" records each completed
// turn's (input, answer) so the transcript can be rebuilt. Intermediate tool calls stay
// in the turn's journal and are NOT carried into later turns — the conversational memory
// is the question/answer transcript, not every tool call.
type Session struct {
	agent   *Agent
	id      string
	history []Message // alternating user / final-assistant messages
	turns   int
	keyed   map[string]turnRecord // completed SendOnce turns by key
}

// turnRecord is the journaled shape of one completed conversation turn.
type turnRecord struct {
	Input  string  `json:"input"`
	Answer Message `json:"answer"`
	Key    string  `json:"key,omitempty"` // SendOnce's key; empty for Send
}

func sessionTurnStep(n int) string { return "turn/" + strconv.Itoa(n) }

// Session opens (or reopens) a multi-turn conversation with the given id, rebuilding the
// transcript from the store so a restarted process continues where it left off.
func (a *Agent) Session(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, fmt.Errorf("Session: empty id: %w", ErrConfig)
	}
	recs, err := a.store.History(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("session %s: load transcript: %w (%w)", id, err, ErrStorage)
	}
	s := &Session{agent: a, id: id, keyed: map[string]turnRecord{}}
	for _, r := range recs {
		if r.Kind != StepValue || r.Name != sessionTurnStep(s.turns) {
			continue
		}
		var tr turnRecord
		if err := json.Unmarshal(r.Result, &tr); err != nil {
			return nil, fmt.Errorf("session %s: decode turn %d: %w (%w)", id, s.turns, err, ErrProtocol)
		}
		s.history = append(s.history, UserText(tr.Input), tr.Answer)
		s.turns++
		if tr.Key != "" {
			s.keyed[tr.Key] = tr
		}
	}
	return s, nil
}

// Send runs one conversation turn: the agent answers `input` with the full prior
// transcript in context, and the turn is journaled. Returns the assistant's answer.
//
// If the turn pauses (a tool needs approval or Interrupt), Send returns that error
// (*PendingApproval / *Interrupted) and does NOT advance the transcript; resolve it
// (Approve / Resume) and call Send again with the SAME input to resume that turn.
func (s *Session) Send(ctx context.Context, input string) (Message, error) {
	return s.send(ctx, s.id+"/t"+strconv.Itoa(s.turns), "", input)
}

// SendOnce runs one conversation turn for an inbound message identified by key (an event or
// message id), at most once per key. A key whose turn already completed returns that turn's
// answer without running anything, so a redelivered message never opens a second turn, even
// if the process died after the turn was recorded and before the caller replied. A key whose
// turn was interrupted resumes that same turn: it runs under its own journal,
// "<session id>/e/<key>", rather than the next turn index. Reusing a key with a different
// input is ErrConfig.
//
// Like Send, it assumes one writer per session at a time; deliver a conversation's messages in
// order.
func (s *Session) SendOnce(ctx context.Context, key, input string) (Message, error) {
	if key == "" {
		return Message{}, fmt.Errorf("session %s: SendOnce: empty key: %w", s.id, ErrConfig)
	}
	if tr, ok := s.keyed[key]; ok {
		if tr.Input != input {
			return Message{}, fmt.Errorf("session %s: key %q was already used for a different message: %w", s.id, key, ErrConfig)
		}
		return tr.Answer, nil
	}
	return s.send(ctx, s.id+"/e/"+key, key, input)
}

func (s *Session) send(ctx context.Context, turnRunID, key, input string) (Message, error) {
	seed := make([]Message, 0, len(s.history)+1)
	seed = append(seed, s.history...)
	seed = append(seed, UserText(input))

	answer, _, _, err := s.agent.run(ctx, turnRunID, seed, false, nil)
	if err != nil {
		return answer, err // pause/error: transcript unadvanced; retry same input to resume
	}

	// Journal the completed turn so the transcript survives a restart.
	rec := turnRecord{Input: input, Answer: answer, Key: key}
	tr, err := json.Marshal(rec)
	if err != nil {
		return answer, fmt.Errorf("session %s: encode turn %d: %w (%w)", s.id, s.turns, err, ErrConfig)
	}
	if _, err := s.agent.store.Do(ctx, s.id, sessionTurnStep(s.turns), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: tr}, nil
	}); err != nil {
		return answer, fmt.Errorf("session %s: record turn %d: %w (%w)", s.id, s.turns, err, ErrStorage)
	}
	s.history = append(s.history, UserText(input), answer)
	s.turns++
	if key != "" {
		s.keyed[key] = rec
	}
	return answer, nil
}

// History returns a copy of the conversation transcript so far (alternating user and
// final-assistant messages).
func (s *Session) History() []Message {
	out := make([]Message, len(s.history))
	copy(out, s.history)
	return out
}

// Turns returns the number of completed turns.
func (s *Session) Turns() int { return s.turns }
