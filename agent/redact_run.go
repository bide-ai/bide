package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RedactionSelector chooses the records of one run a redaction replaces. A record is selected if it
// matches any of Names (exact keys), Kinds, or Prefixes (key prefixes), or Payloads is set and it is
// payload; a protected record (RedactionProtected) is never selected, and appears in the plan as
// kept, with the reason.
type RedactionSelector struct {
	Names    []string
	Kinds    []StepKind
	Prefixes []string
	// Payloads selects every record whose content is payload: run:start (the input and the
	// principal), model turns, tool results, user steps, signals and approval requests, except the
	// usage-only @spend records. It never selects an approval decision (StepApproval) unless
	// IncludeApprovals is set, and never a protected record.
	Payloads bool
	// IncludeApprovals lets Payloads, Kinds and Prefixes select approval decisions (StepApproval).
	// An approval decision is accountability evidence about the people who decided, which
	// verify-approvals and decision coverage read, so it is kept unless this is set.
	IncludeApprovals bool
}

// PlannedRedaction is one record a redaction would replace (or already replaced).
type PlannedRedaction struct {
	// Position is the record's index in the run's journal, in Load order.
	Position int    `json:"position"`
	Name     string `json:"name"`
	// Kind is the record's kind, empty for a record already redacted.
	Kind StepKind `json:"kind,omitempty"`
	// Length is the stored length in bytes, 0 for a record already redacted.
	Length int `json:"length,omitempty"`
	// LeafHash is the hex audit leaf hash of the stored bytes: the hash the tombstone keeps.
	LeafHash string `json:"leaf_hash"`
	// AlreadyRedacted is set for a record that already holds a tombstone.
	AlreadyRedacted bool `json:"already_redacted,omitempty"`
}

// KeptRecord is a record a selector chose and a redaction keeps, and why.
type KeptRecord struct {
	Position int    `json:"position"`
	Name     string `json:"name"`
	Reason   string `json:"reason"`
}

// RedactionPlan is what a redaction of one run would do: the records it replaces, the selected
// records it keeps, and a digest of the replacements. It holds no record content. Digest is a
// SHA-256 over the run ID and each replaced record's position, name and leaf hash, so a person who
// reviewed a plan can name exactly what may be executed; it does not change when part of the plan
// is already done.
type RedactionPlan struct {
	RunID   string             `json:"run_id"`
	Records []PlannedRedaction `json:"records"`
	Kept    []KeptRecord       `json:"kept,omitempty"`
	// JournalSize is the number of records the run held when it was planned.
	JournalSize int `json:"journal_size"`
	// Terminal is whether the run held an end marker when it was planned. A store refuses a
	// redaction of a run that does not.
	Terminal bool   `json:"terminal"`
	Digest   string `json:"digest"`
}

// PlanRedaction reads runID's journal and returns what redacting the records sel selects would do,
// without writing anything. A run that is not over still gets a plan (Terminal false), so a dry run
// shows what would be refused.
func PlanRedaction(ctx context.Context, j *Journal, runID string, sel RedactionSelector) (RedactionPlan, error) {
	if j == nil {
		return RedactionPlan{}, fmt.Errorf("PlanRedaction: nil journal: %w", ErrConfig)
	}
	recs, err := j.History(ctx, runID)
	if err != nil {
		return RedactionPlan{}, err
	}
	p := RedactionPlan{RunID: runID, JournalSize: len(recs), Terminal: terminal(recs)}
	for i, r := range recs {
		if r.Redacted {
			// A tombstone keeps no kind, so a selector by kind or preset takes every tombstone of
			// the run as done: only a redaction wrote it, and only a redactable record is redacted.
			if !sel.matchesName(r.Name) && !sel.Payloads && len(sel.Kinds) == 0 {
				continue
			}
			leaf, ok := TombstoneLeafHash(r.Raw())
			if !ok {
				return RedactionPlan{}, fmt.Errorf("run %s, record %d (%q): its tombstone does not read: %w", runID, i, r.Name, ErrStorage)
			}
			p.Records = append(p.Records, PlannedRedaction{Position: i, Name: r.Name, LeafHash: hex.EncodeToString(leaf), AlreadyRedacted: true})
			continue
		}
		explicit, preset := sel.matchesName(r.Name) || slices.Contains(sel.Kinds, r.Kind), sel.preset(r)
		if !explicit && !preset {
			continue
		}
		if why := RedactionProtected(r.Name, r.Kind); why != "" {
			// A record only the preset reached is not listed: it is not what the operator asked for.
			if explicit {
				p.Kept = append(p.Kept, KeptRecord{i, r.Name, "protected: " + why})
			}
			continue
		}
		if r.Kind == StepApproval && !sel.IncludeApprovals {
			p.Kept = append(p.Kept, KeptRecord{i, r.Name, "an approval decision is kept unless IncludeApprovals is set"})
			continue
		}
		raw := r.Raw()
		p.Records = append(p.Records, PlannedRedaction{Position: i, Name: r.Name, Kind: r.Kind, Length: len(raw), LeafHash: hex.EncodeToString(LeafHash(raw))})
	}
	p.Digest = p.digest()
	return p, nil
}

// terminal reports whether recs hold an end marker, by name: a tombstoned end marker cannot occur
// (end markers are protected), and a run that holds one is over.
func terminal(recs []Record) bool {
	_, ok := firstEnd(recs)
	return ok
}

// matchesName reports whether a record named name is chosen by name or prefix.
func (s RedactionSelector) matchesName(name string) bool {
	if slices.Contains(s.Names, name) {
		return true
	}
	for _, p := range s.Prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// preset reports whether the Payloads preset chooses r.
func (s RedactionSelector) preset(r Record) bool {
	if !s.Payloads {
		return false
	}
	switch r.Kind {
	case StepModel, StepToolResult, StepSignal, StepApproval:
		return true
	case StepValue:
		// Usage-only @spend records and the approval bookkeeping records hold no payload; an
		// approval request does.
		if strings.HasPrefix(r.Name, "@spend") {
			return false
		}
		return !strings.HasPrefix(r.Name, "approval") || strings.HasPrefix(r.Name, "approval-request:")
	}
	return false
}

// digest is the plan's Digest.
func (p RedactionPlan) digest() string {
	h := sha256.New()
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	field([]byte("bide.redaction-plan.v1"))
	field([]byte(p.RunID))
	for _, r := range p.Records {
		var pos [8]byte
		binary.BigEndian.PutUint64(pos[:], uint64(r.Position))
		field(pos[:])
		field([]byte(r.Name))
		field([]byte(r.LeafHash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RedactionResult is what Redact did.
type RedactionResult struct {
	// Redacted are the records this call replaced, and Already those that already held a
	// tombstone (replaced by an earlier pass), both by position.
	Redacted []PlannedRedaction `json:"redacted,omitempty"`
	Already  []PlannedRedaction `json:"already,omitempty"`
}

// Redact replaces the records plan names with tombstones, in position order, each in its own
// transaction of the store's Redactor (so a crash leaves a prefix replaced and a second call with
// the same plan finishes the rest). For each record it reads the stored bytes, refuses one whose
// leaf hash is not the planned one (ErrRedactChanged: the run changed since it was planned), builds
// the tombstone (NewTombstone, stamped at) and calls the store, which re-checks everything
// (Redactor.Redact). It stops at the first error and returns what it had done. It returns an
// ErrConfig error if the journal's store has no Redactor.
//
// Redact does not account for what it did: a redaction that must be audited writes the record of it
// before calling Redact and after it returns (see docs/design/journal-redaction.md).
func Redact(ctx context.Context, j *Journal, plan RedactionPlan, at time.Time) (RedactionResult, error) {
	var res RedactionResult
	if j == nil {
		return res, fmt.Errorf("Redact: nil journal: %w", ErrConfig)
	}
	store, ok := Capability[Redactor](j.store)
	if !ok {
		return res, fmt.Errorf("Redact: the journal's store cannot redact (it does not implement agent.Redactor): %w", ErrConfig)
	}
	if plan.Digest != plan.digest() {
		return res, fmt.Errorf("Redact: the plan's digest does not match its records: %w", ErrConfig)
	}
	for _, pr := range plan.Records {
		e, found, err := j.store.Get(ctx, plan.RunID, pr.Name)
		if err != nil {
			return res, storageErr(fmt.Sprintf("read %q of run %s", pr.Name, plan.RunID), err)
		}
		if !found {
			return res, fmt.Errorf("run %s, entry %q: %w", plan.RunID, pr.Name, ErrRedactNoEntry)
		}
		if leaf, ok := TombstoneLeafHash(e.Data); ok {
			if hex.EncodeToString(leaf) != pr.LeafHash {
				return res, fmt.Errorf("run %s, entry %q holds a tombstone for another record than the plan names: %w", plan.RunID, pr.Name, ErrRedactChanged)
			}
			res.Already = append(res.Already, pr)
			continue
		}
		if hex.EncodeToString(LeafHash(e.Data)) != pr.LeafHash {
			return res, fmt.Errorf("run %s, entry %q is not the record the plan was made from: %w", plan.RunID, pr.Name, ErrRedactChanged)
		}
		tomb, err := NewTombstone(pr.Name, e.Data, at)
		if err != nil {
			return res, err
		}
		replaced, err := store.Redact(ctx, plan.RunID, pr.Name, bytes.Clone(e.Data), tomb)
		if err != nil {
			return res, err
		}
		if replaced {
			res.Redacted = append(res.Redacted, pr)
		} else {
			res.Already = append(res.Already, pr)
		}
	}
	return res, nil
}
