package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// This file is the event→audit sink: it turns Agent.Stream's ephemeral SEMANTIC lifecycle
// events (AgentEvents — turn boundaries, tool start/finish, approvals, the final answer)
// into a verifiable, tamper-evident log. Root/Head/Prove in this package commit over the
// durable JOURNAL after a run; an EventLog commits over the live EVENT STREAM as it happens,
// with the SAME RFC 6962 machinery (merkleRoot / auditPath / verifyPath) and the same
// anchoring caveat (sign/publish the root out-of-band to be tamper-evident, see Head's doc).
//
// Why both: the journal is the source of truth for RESUME; the event log is the source of
// truth for "what did the operator/UI observe, in order, and can they prove a single event
// without revealing the rest." Feeding the stream through an EventLog makes "stream for the
// UI" and "commit an audit trail" one pass instead of two mechanisms.

// eventDomain separates the event-log hash chain from the journal chain (audit.domain).
var eventDomain = sha256.Sum256([]byte("bide.audit.events.v1"))

// EventLog is an append-only, tamper-evident log of one run's AgentEvents. Build it by
// Add-ing events in emission order (Agent.Stream emits them ordered from a single
// goroutine); then Root/Head to commit, Prove for selective disclosure, and audit.Sign to
// anchor. Not safe for concurrent Add — feed it from the one goroutine ranging Events.
type EventLog struct {
	leaves [][]byte
	salts  [][]byte // salts[i] is the salt leaves[i] commits to, disclosed only by Prove(i)
}

// NewEventLog returns an empty log.
func NewEventLog() *EventLog { return &EventLog{} }

// Add appends one event as a canonical, kind-tagged leaf that commits to a fresh random salt
// (agent.SaltSize bytes from crypto/rand; see EventInclusion). It errors if the event cannot be
// canonicalized (it holds a string that is not valid UTF-8, for one) or the system's random
// source fails.
func (l *EventLog) Add(e agent.AgentEvent) error {
	salt, err := newEventSalt()
	if err != nil {
		return err
	}
	return l.add(e, salt)
}

func (l *EventLog) add(e agent.AgentEvent, salt []byte) error {
	b, err := canonicalEvent(e, salt)
	if err != nil {
		return err
	}
	l.leaves = append(l.leaves, b)
	l.salts = append(l.salts, append([]byte(nil), salt...))
	return nil
}

// Len is the number of events committed so far.
func (l *EventLog) Len() int { return len(l.leaves) }

// Root is the RFC 6962 Merkle root over every event added so far (empty-log root =
// SHA-256(); identical shape to audit.Root over the journal). Supports Prove.
func (l *EventLog) Root() []byte { return merkleRoot(l.leaves) }

// Head is the linear SHA-256 hash-chain commitment over the events (domain-separated),
// the event-stream analogue of audit.Head: head_0 = H(eventDomain), head_i =
// H(head_{i-1} || canonical(event_i)). Cheaper than Root, no selective disclosure.
func (l *EventLog) Head() []byte {
	head := eventDomain[:]
	for _, leaf := range l.leaves {
		sum := sha256.Sum256(append(append([]byte{}, head...), leaf...))
		head = sum[:]
	}
	return head
}

// EventInclusion is an inclusion proof for one event: the event's own salt, which its leaf
// commits to, and the RFC 6962 audit path (the same Inclusion journal proofs use) from that leaf
// to the log's Root. It discloses the proven event's salt, its index, the log's size, and the
// sibling hashes on its path. Each sibling hash covers other events' leaves, and every leaf
// commits to its own random salt, so a holder of the proof cannot confirm a guess about any other
// event by hashing it: the salt a guess would need is disclosed only by that event's own proof.
type EventInclusion struct {
	Format string `json:"format"` // EventInclusionFormat
	Salt   []byte `json:"salt"`   // the proven event's salt (agent.SaltSize bytes)
	Inclusion
}

// Prove returns an inclusion proof for the event at index against Root: enough to verify that
// one event WITHOUT revealing any other (selective disclosure over the event stream). The proof
// carries the event's salt and nothing about any other event beyond the hashes on its path (see
// EventInclusion). audit.Sign over Root and this proof compose exactly as they do for the journal.
func (l *EventLog) Prove(index int) (EventInclusion, error) {
	if index < 0 || index >= len(l.leaves) {
		return EventInclusion{}, fmt.Errorf("audit: event index %d out of range [0,%d)", index, len(l.leaves))
	}
	return EventInclusion{
		Format:    EventInclusionFormat,
		Salt:      append([]byte(nil), l.salts[index]...),
		Inclusion: Inclusion{Index: index, Size: len(l.leaves), Path: auditPath(index, l.leaves)},
	}, nil
}

// TreeHead returns a commitment to runID's events added so far at the given timestamp, the
// EventLog analogue of NewTreeHead over the journal. Its Kind is TreeEvents, so a signed event
// head never verifies as a journal head. Sign it with SignTreeHead and verify with
// SignedTreeHead.Verify; the signature binds kind, run, root, size, and timestamp, so an event
// trail gets the same anchored guarantee a journal STH gives. Inclusion proofs (Prove /
// VerifyEventInclusion) check against the resulting Root; Size counts events.
func (l *EventLog) TreeHead(runID string, timestamp int64) TreeHead {
	return TreeHead{Kind: TreeEvents, RunID: runID, Size: len(l.leaves), Root: merkleRoot(l.leaves), Timestamp: timestamp}
}

// ProveConsistency proves the first `first` events are an append-only PREFIX of the current
// log — nothing observed earlier was rewritten or reordered, only appended. Verify with the
// shared VerifyConsistency against two published event-log roots (e.g. two STH Roots taken
// at different points in the run). This is the transparency-log guarantee over the event
// stream, the same one ProveConsistency gives over the journal.
func (l *EventLog) ProveConsistency(first int) (Consistency, error) {
	if first < 0 || first > len(l.leaves) {
		return Consistency{}, fmt.Errorf("audit: first %d out of range [0,%d]", first, len(l.leaves))
	}
	return Consistency{First: first, Size: len(l.leaves), Path: consistencyProof(first, l.leaves)}, nil
}

// VerifyEventInclusion reports whether event, under the salt proof discloses, is the leaf at
// proof.Index in a log of proof.Size events committed by root: from the event and proof alone,
// no other events needed. The event must canonicalize identically to when it was added. It
// errors if the event cannot be canonicalized, proof.Salt is not agent.SaltSize bytes, or
// proof.Format is not EventInclusionFormat (ErrFormat).
func VerifyEventInclusion(root []byte, event agent.AgentEvent, proof EventInclusion) (bool, error) {
	if err := formatOf(proof, proof.Format); err != nil {
		return false, err
	}
	leaf, err := canonicalEvent(event, proof.Salt)
	if err != nil {
		return false, err
	}
	return verifyPath(root, leaf, proof.Index, proof.Size, proof.Path), nil
}

// EventLogFromJournal builds an EventLog from runID's DURABLE journal by projecting it to the
// semantic events Agent.Stream re-emits on resume (agent.ReplayEvents). This is the
// crash-durable, resume-stable counterpart to filling an EventLog from the live stream: it is
// a deterministic function of the persisted journal, so its Root / STH are byte-identical
// before and after a crash. THIS is the artifact to anchor for a durable audit trail; the
// live-stream EventLog is a real-time view (and its Root shifts between a fresh run and its
// replay because live-only events differ). Root / Head / TreeHead / Prove / ProveConsistency
// then work exactly as they do on any EventLog.
//
// Each projected event comes from one journal record, and its salt is derived from that
// record's random salt (journalEventSalt), so the projection needs no state beyond the journal.
// It errors if a source record has no agent.SaltSize salt.
func EventLogFromJournal(ctx context.Context, store agent.Durable, runID string) (*EventLog, error) {
	evs, salts, err := projectJournal(ctx, store, runID)
	if err != nil {
		return nil, err
	}
	log := NewEventLog()
	for i, e := range evs {
		if err := log.add(e, salts[i]); err != nil {
			return nil, err
		}
	}
	return log, nil
}

// eventSaltTag separates a projected event's salt from its source record's salt.
const eventSaltTag = "bide.audit.event-salt.v1\x00"

// journalEventSalt is the salt of the event projected from a journal record whose salt is
// recordSalt: SHA-256("bide.audit.event-salt.v1\x00" || recordSalt). The record's salt is random
// and every store persists it with the record, so the event's salt is as unguessable, is the same
// on every projection of the journal, and needs no storage of its own. The hash is one-way: an
// event proof discloses this salt but not the record's, so it does not open the record's journal
// leaf.
func journalEventSalt(recordSalt []byte) []byte {
	sum := sha256.Sum256(append([]byte(eventSaltTag), recordSalt...))
	return sum[:]
}

// projectJournal returns the events agent.ReplayEvents returns for runID's journal and each
// event's salt, derived from the record the event projects (journalEventSalt).
func projectJournal(ctx context.Context, store agent.Durable, runID string) ([]agent.AgentEvent, [][]byte, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("audit: load history %s: %w", runID, err)
	}
	evs, sources := agent.ProjectEvents(recs)
	salts := make([][]byte, len(evs))
	for i, src := range sources {
		r := recs[src]
		if len(r.Salt) != agent.SaltSize {
			return nil, nil, fmt.Errorf("audit: run %s: record %q has a %d-byte salt, want %d (a store sets it when it journals the record; see agent.JournalEntry)", runID, r.Name, len(r.Salt), agent.SaltSize)
		}
		salts[i] = journalEventSalt(r.Salt)
	}
	return evs, salts, nil
}

// Record drains stream through log — committing every event — while forwarding each event
// to onEvent (nil to skip), then returns the run's terminal Message and error (including
// *ApprovalPending / *OutcomeUnknown, exactly as AgentStream.Final does). One pass gives you
// both the live UI feed and a committed, provable audit trail. A canonicalization failure
// is surfaced only if the run itself did not already fail.
func Record(log *EventLog, stream *agent.AgentStream, onEvent func(agent.AgentEvent)) (agent.Message, error) {
	var addErr error
	for e := range stream.Events() {
		if err := log.Add(e); err != nil && addErr == nil {
			addErr = err
		}
		if onEvent != nil {
			onEvent(e)
		}
	}
	msg, err := stream.Final()
	if err != nil {
		return msg, err
	}
	return msg, addErr
}

// eventLeaf is the canonical wire form of one event: a kind tag, the event's JSON, and the
// event's random salt (base64). The kind makes the leaf self-describing so two different event
// types can never collide by having the same field shape (e.g. an empty struct); the salt makes
// the leaf's hash unguessable from the event's content (see EventInclusion).
type eventLeaf struct {
	Kind  string          `json:"kind"`
	Event json.RawMessage `json:"event"`
	Salt  []byte          `json:"salt"`
}

// canonicalEvent returns the leaf bytes of e under salt:
// "bide.audit.event-leaf.v2\x00" || {"kind":...,"event":...,"salt":...}. It refuses a salt that
// is not agent.SaltSize bytes: the leaf would be guessable from the event's content. It refuses an
// event holding a string that is not valid UTF-8 (see checkUTF8): the encoding would rewrite it,
// so two different events would share one leaf and a proof of one would verify the other.
func canonicalEvent(e agent.AgentEvent, salt []byte) ([]byte, error) {
	if len(salt) != agent.SaltSize {
		return nil, fmt.Errorf("audit: canonicalize event: %d-byte salt, want %d", len(salt), agent.SaltSize)
	}
	if err := checkUTF8(e); err != nil {
		return nil, fmt.Errorf("audit: canonicalize event: %w", err)
	}
	inner, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("audit: canonicalize event: %w", err)
	}
	b, err := json.Marshal(eventLeaf{Kind: eventKind(e), Event: inner, Salt: salt})
	if err != nil {
		return nil, fmt.Errorf("audit: canonicalize event: %w", err)
	}
	return tagged(eventLeafTag, b), nil
}

// eventLeafSalt returns the salt a stored event leaf commits to. It refuses a leaf of another
// kind or version (an unsalted bide.audit.event-leaf.v1 leaf among them) and one whose salt is
// not agent.SaltSize bytes.
func eventLeafSalt(leaf []byte) ([]byte, error) {
	body, ok := bytes.CutPrefix(leaf, []byte(eventLeafTag))
	if !ok {
		return nil, fmt.Errorf("not a %s leaf", eventLeafTag[:len(eventLeafTag)-1])
	}
	var el eventLeaf
	if err := json.Unmarshal(body, &el); err != nil {
		return nil, err
	}
	if len(el.Salt) != agent.SaltSize {
		return nil, fmt.Errorf("%d-byte salt, want %d", len(el.Salt), agent.SaltSize)
	}
	return el.Salt, nil
}

// newEventSalt returns agent.SaltSize bytes from crypto/rand.
func newEventSalt() ([]byte, error) {
	salt := make([]byte, agent.SaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("audit: salt event: %w", err)
	}
	return salt, nil
}

// eventKind is a stable, human-readable discriminator for an AgentEvent. ModelEvent carries
// the inner model event's kind too, so token/reasoning/tool-call deltas stay distinct even
// when their JSON coincides.
func eventKind(e agent.AgentEvent) string {
	switch ev := e.(type) {
	case agent.TurnStarted:
		return "TurnStarted"
	case agent.ModelEvent:
		return "ModelEvent/" + modelEventKind(ev.Event)
	case agent.AssistantTurn:
		return "AssistantTurn"
	case agent.ToolStarted:
		return "ToolStarted"
	case agent.ToolCompleted:
		return "ToolCompleted"
	case agent.ApprovalRequired:
		return "ApprovalRequired"
	case agent.Finished:
		return "Finished"
	default:
		return fmt.Sprintf("%T", e)
	}
}

func modelEventKind(e agent.Event) string {
	switch e.(type) {
	case agent.TextDelta:
		return "TextDelta"
	case agent.ReasoningDelta:
		return "ReasoningDelta"
	case agent.ToolCallDelta:
		return "ToolCallDelta"
	case agent.Finish:
		return "Finish"
	default:
		return fmt.Sprintf("%T", e)
	}
}
