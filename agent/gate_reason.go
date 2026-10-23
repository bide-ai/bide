package agent

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/bide-ai/bide/internal/strictjson"
)

// The gate-reason record (docs/design/untrusted-content-and-amount-caps.md, section 5.3) says why a
// call needs a gate beyond its tool's own: because its turn had read untrusted content (taint), or a
// cap rule applies. It is evidence for an approver and an auditor, and it is never a source of
// authority: the engine recomputes the reasons from the journal and the registered spec on every
// evaluation and enforces the union of those and the recorded ones (unionReasons), so a record can add
// a requirement and never remove one. Nothing here is reached by the engine yet; taint and caps will
// write and read it.

// gateReasonPrefix is the name prefix of the record, reserved to the engine.
const gateReasonPrefix = "gate-reason:"

// gateReasonFormat tags the record.
const gateReasonFormat = "bide.gate-reason.v1"

// gateReasonStep is the journal name of toolUseID's gate-reason record.
func gateReasonStep(toolUseID string) string { return gateReasonPrefix + encodeID(toolUseID) }

// gateReasonKind says what kind of reason a gateReason is.
type gateReasonKind string

const (
	gateReasonTaint gateReasonKind = "taint" // the requesting turn had read untrusted content
	gateReasonCap   gateReasonKind = "cap"   // a cap rule applies
)

// gateReason is one reason. For a taint reason Source is the declared source name, Ref where it was read
// (tool and call, "input" or "retrieval") and Pos the position of the mark in the run's journal; for a
// cap reason Rule is the rule's name.
type gateReason struct {
	Kind   gateReasonKind `json:"kind"`
	Source string         `json:"source,omitempty"`
	Ref    string         `json:"ref,omitempty"`
	Pos    int            `json:"pos,omitempty"`
	Rule   string         `json:"rule,omitempty"`
}

// key is a total, unambiguous identity of the reason, for the union (length-prefixed, so no two
// distinct reasons share a key).
func (r gateReason) key() string {
	var b strings.Builder
	for _, s := range []string{string(r.Kind), r.Source, r.Ref, strconv.Itoa(r.Pos), r.Rule} {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	return b.String()
}

// gateReasonRecord is what the record named gateReasonStep holds. Exempt is the reason text of a
// registered exemption, kept for the audit only: nothing reads it to decide (the exemption is derived
// from the registered spec). PolicyDigest is the digest of the escalation policy at the ask.
type gateReasonRecord struct {
	Format       string       `json:"format"`
	ToolUseID    string       `json:"tool_use_id"`
	Reasons      []gateReason `json:"reasons,omitempty"`
	PolicyDigest string       `json:"policy_digest,omitempty"`
	Exempt       string       `json:"exempt,omitempty"`
	RecordedAt   string       `json:"recorded_at,omitempty"`
}

// unionReasons is the reasons a call is gated for: those the engine recomputed now and those a record
// holds, each once, in a fixed order. It is symmetric and idempotent, and no input can remove a reason
// another holds: a record that is empty, wrong or planted by a writer of the store adds at most
// requirements. A reason of a kind this version does not know is kept.
func unionReasons(recomputed, recorded []gateReason) []gateReason {
	seen := map[string]bool{}
	var out []gateReason
	for _, r := range slices.Concat(recomputed, recorded) {
		if k := r.key(); !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b gateReason) int { return strings.Compare(a.key(), b.key()) })
	return out
}

// putGateReason records rec, insert-once, and returns the record the journal holds: rec when this call
// wrote it, else the winner's, and whether this call inserted it. The winner is read strictly.
func putGateReason(ctx context.Context, j *Journal, runID string, rec gateReasonRecord) (gateReasonRecord, bool, error) {
	b, err := marshalJournal(rec)
	if err != nil {
		return gateReasonRecord{}, false, fmt.Errorf("encode %s (run %s): %w (%w)", gateReasonStep(rec.ToolUseID), runID, err, ErrConfig)
	}
	got, inserted, err := j.putNew(ctx, runID, gateReasonStep(rec.ToolUseID), Record{Kind: StepValue, Result: b})
	if err != nil {
		return gateReasonRecord{}, false, fmt.Errorf("record %s (run %s): %w", gateReasonStep(rec.ToolUseID), runID, err)
	}
	have, err := decodeGateReason(runID, rec.ToolUseID, got)
	return have, inserted, err
}

// getGateReason reads toolUseID's gate-reason record: ok is false when there is none. A record that
// does not read strictly, as decodeTally reads a tally (a duplicate, case-variant or unknown name at
// any depth, another format, another call's ID), is an ErrStorage refusal. Unknown names are refused
// rather than ignored because a binary that ignored a field a later version adds could drop a reason
// kind it cannot recompute, which would weaken the gate; the record evolves through its format name: the caller does not run the call on it, and the
// reasons it would have held are not lost to a repair, since the engine recomputes them.
func getGateReason(ctx context.Context, j *Journal, runID, toolUseID string) (gateReasonRecord, bool, error) {
	got, ok, err := j.Get(ctx, runID, gateReasonStep(toolUseID))
	if err != nil || !ok {
		return gateReasonRecord{}, false, err
	}
	rec, err := decodeGateReason(runID, toolUseID, got)
	if err != nil {
		return gateReasonRecord{}, false, err
	}
	return rec, true, nil
}

func decodeGateReason(runID, toolUseID string, r Record) (gateReasonRecord, error) {
	var rec gateReasonRecord
	if err := strictjson.Unmarshal(r.Result, &rec, nil); err != nil || rec.Format != gateReasonFormat || rec.ToolUseID != toolUseID {
		return gateReasonRecord{}, fmt.Errorf("run %s: the record %s does not read as a %s record for call %q: %w", runID, gateReasonStep(toolUseID), gateReasonFormat, toolUseID, ErrStorage)
	}
	return rec, nil
}
