package audit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	agent "github.com/dayna/go-agents"
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
var eventDomain = sha256.Sum256([]byte("go-agents.audit.events.v1"))

// EventLog is an append-only, tamper-evident log of one run's AgentEvents. Build it by
// Add-ing events in emission order (Agent.Stream emits them ordered from a single
// goroutine); then Root/Head to commit, Prove for selective disclosure, and audit.Sign to
// anchor. Not safe for concurrent Add — feed it from the one goroutine ranging Events.
type EventLog struct {
	leaves [][]byte
}

// NewEventLog returns an empty log.
func NewEventLog() *EventLog { return &EventLog{} }

// Add appends one event as a canonical, kind-tagged leaf.
func (l *EventLog) Add(e agent.AgentEvent) error {
	b, err := canonicalEvent(e)
	if err != nil {
		return err
	}
	l.leaves = append(l.leaves, b)
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

// Prove returns an inclusion proof for the event at index against Root — enough to verify
// that one event WITHOUT revealing any other (selective disclosure over the event stream).
// The returned Inclusion is the same type journal proofs use, so audit.Sign over Root and
// this proof compose exactly as they do for the journal.
func (l *EventLog) Prove(index int) (Inclusion, error) {
	if index < 0 || index >= len(l.leaves) {
		return Inclusion{}, fmt.Errorf("audit: event index %d out of range [0,%d)", index, len(l.leaves))
	}
	return Inclusion{Index: index, Size: len(l.leaves), Path: auditPath(index, l.leaves)}, nil
}

// TreeHead returns a commitment to the events added so far at the given timestamp — the
// EventLog analogue of NewTreeHead over the journal. Sign it with SignTreeHead and verify
// with SignedTreeHead.Verify; the signature binds Root ↔ Size ↔ Timestamp, so an event
// trail gets the same anchored root↔size↔time guarantee a journal STH gives. Inclusion
// proofs (Prove / VerifyEventInclusion) check against the resulting Root; Size counts events.
func (l *EventLog) TreeHead(timestamp int64) TreeHead {
	return TreeHead{Size: len(l.leaves), Root: merkleRoot(l.leaves), Timestamp: timestamp}
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

// VerifyEventInclusion reports whether event is the leaf at proof.Index in a log of
// proof.Size events committed by root — from the event + proof alone, no other events
// needed. The event must canonicalize identically to when it was Add-ed.
func VerifyEventInclusion(root []byte, event agent.AgentEvent, proof Inclusion) (bool, error) {
	leaf, err := canonicalEvent(event)
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
func EventLogFromJournal(ctx context.Context, store agent.Durable, runID string) (*EventLog, error) {
	evs, err := agent.ReplayEvents(ctx, store, runID)
	if err != nil {
		return nil, err
	}
	log := NewEventLog()
	for _, e := range evs {
		if err := log.Add(e); err != nil {
			return nil, err
		}
	}
	return log, nil
}

// Record drains stream through log — committing every event — while forwarding each event
// to onEvent (nil to skip), then returns the run's terminal Message and error (including
// *PendingApproval / *ResumeHalt, exactly as AgentStream.Final does). One pass gives you
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

// eventLeaf is the canonical wire form of one event: a kind tag plus the event's JSON.
// The tag makes the leaf self-describing so two different event types can never collide by
// having the same field shape (e.g. an empty struct).
type eventLeaf struct {
	Kind  string          `json:"kind"`
	Event json.RawMessage `json:"event"`
}

func canonicalEvent(e agent.AgentEvent) ([]byte, error) {
	inner, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("audit: canonicalize event: %w", err)
	}
	return json.Marshal(eventLeaf{Kind: eventKind(e), Event: inner})
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
