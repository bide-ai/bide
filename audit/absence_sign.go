package audit

import (
	"github.com/bide-ai/bide/agent"
)

// ToolUseKeyFor is the absence key for a tool-use id: pass it to ProveAbsent / ProveAbsentBundle
// with ToolUseKeys to prove no tool call with that id happened in the run. It mirrors
// PolicyUsedKeyFor for the PolicyUsedKeys key set.
func ToolUseKeyFor(toolUseID string) string { return toolUseKeyPrefix + toolUseID }

// NewAbsenceTreeHead builds the key-set tree head for set over a run's journal. journal is the
// run's journal tree head (NewTreeHead) and records the run's journal: the first journal.Size
// records must hash to journal.Root. The head commits to set.Kind, the run, the key set projected
// from exactly those records, and the journal tree itself, so it is bound to one run's history.
func NewAbsenceTreeHead(records []agent.Record, set KeySet, journal TreeHead, timestamp int64) (TreeHead, error) {
	if err := set.check(); err != nil {
		return TreeHead{}, err
	}
	recs, err := journalPrefix(journal.RunID, records, journal)
	if err != nil {
		return TreeHead{}, err
	}
	if err := refuseRedacted(recs, "the "+set.Kind+" key set"); err != nil {
		return TreeHead{}, err
	}
	keys := absenceKeys(recs, set)
	return TreeHead{
		Kind:           set.Kind,
		RunID:          journal.RunID,
		Size:           len(keys),
		Root:           merkleRoot(keyLeaves(keys)),
		TimestampNanos: timestamp,
		Journal:        &TreeRef{Size: journal.Size, Root: append([]byte(nil), journal.Root...)},
	}, nil
}

// SignAbsenceRoot commits and signs a run's key set in one call: NewAbsenceTreeHead, then
// SignTreeHead with s. Absence proofs verify against this separate commitment (never the journal
// STH). Anchor the result like any STH. timestamp is Unix nanoseconds.
func SignAbsenceRoot(records []agent.Record, set KeySet, journal TreeHead, s Signer, timestamp int64) (SignedTreeHead, error) {
	if err := checkSigner(s); err != nil {
		return SignedTreeHead{}, err
	}
	th, err := NewAbsenceTreeHead(records, set, journal, timestamp)
	if err != nil {
		return SignedTreeHead{}, err
	}
	return SignTreeHead(th, s)
}
