package audit

import (
	"crypto/ed25519"

	agent "github.com/blackwell-systems/bide"
)

// ToolUseKeyFor is the absence key for a tool-use id: pass it to ProveAbsent / ProveAbsentBundle
// with ToolUseKey to prove no tool call with that id happened in the run. It mirrors
// PolicyUsedKeyFor for the ToolUseKey key set.
func ToolUseKeyFor(toolUseID string) string { return "tooluse:" + toolUseID }

// SignAbsenceRoot commits and signs the run's absence key set under keyFn in one call: it builds a
// TreeHead over AbsenceRoot(records, keyFn) and signs it. Absence proofs verify against this
// separate commitment (not the journal STH), so this is the one-step producer for it. Anchor the
// result like any STH; a verifier holding the journal recomputes AbsenceRoot to confirm the STH
// commits to this run's key set before trusting an absence proof against it.
func SignAbsenceRoot(records []agent.Record, keyFn KeyFunc, priv ed25519.PrivateKey, timestamp int64) SignedTreeHead {
	keys := absenceKeys(records, keyFn)
	th := TreeHead{Size: len(keys), Root: merkleRoot(keyLeaves(keys)), Timestamp: timestamp}
	return SignTreeHead(th, priv)
}
