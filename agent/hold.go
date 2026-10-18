package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// A Hold is a legal hold: a promise that the records of some runs are not erased while a matter is
// open. A store that implements Holder keeps the active holds beside the journal, and its Redactor
// refuses, in the same statement that would replace a record, any run a hold covers (ErrRedactHeld).
type Hold struct {
	// ID names the hold; placing a hold with an ID that is active is an error. Lifting removes every
	// run it covers at once.
	ID string `json:"id"`
	// Matter is the matter the hold is for, in the operator's words; recorded, never interpreted.
	Matter string `json:"matter"`
	// Runs are the run IDs the hold covers. With Tree, each also covers the runs whose IDs it prefixes
	// as sub-agent runs ("<run>><call>", at any depth).
	Runs []string `json:"runs"` // read back sorted and without duplicates
	Tree bool     `json:"tree,omitempty"`
	// PlacedAtMs is when the store recorded the hold, in Unix milliseconds.
	PlacedAtMs int64 `json:"placed_at_ms"`
}

// Covers reports whether the hold covers runID: it is one of the hold's runs, or, for a hold with
// Tree, a sub-agent run below one of them.
func (h Hold) Covers(runID string) bool {
	if slices.Contains(h.Runs, runID) {
		return true
	}
	if !h.Tree {
		return false
	}
	for _, anc := range RedactionAncestors(runID) {
		if slices.Contains(h.Runs, anc) {
			return true
		}
	}
	return false
}

// Holder is the optional store capability that keeps legal holds. A store opts in by implementing it
// (MemStore, SQLite and Postgres do); a caller finds it with Capability. Placing and lifting a hold are
// the operator's acts, and the command that makes them records them in the signed redaction trail:
// the store is where the hold is enforced, the trail is where it is evidenced.
type Holder interface {
	// PlaceHold records h, atomically, for every run it names. It refuses a hold with no ID, no
	// runs, a blank run or an ID already active (ErrHoldExists).
	PlaceHold(ctx context.Context, h Hold) error
	// LiftHold removes the hold with this ID and reports whether it was active.
	LiftHold(ctx context.Context, id string) (bool, error)
	// Holds returns the active holds, ordered by ID.
	Holds(ctx context.Context) ([]Hold, error)
}

// ErrHoldExists is a PlaceHold of an ID that is active.
var ErrHoldExists = fmt.Errorf("a hold with this ID is already active: %w", ErrConfig)

// ErrRedactHeld is a redaction of a run an active hold covers.
var ErrRedactHeld = fmt.Errorf("redaction refused: the run is under a legal hold: %w", ErrConfig)

// CheckHold checks the parts of a hold no store needs to decide: an ID, a matter and runs that are
// not blank. Every Holder calls it.
func CheckHold(h Hold) error {
	switch {
	case strings.TrimSpace(h.ID) == "":
		return fmt.Errorf("a hold needs an ID: %w", ErrConfig)
	case strings.TrimSpace(h.Matter) == "":
		return fmt.Errorf("hold %q: a hold needs a matter: %w", h.ID, ErrConfig)
	case len(h.Runs) == 0:
		return fmt.Errorf("hold %q covers no run: %w", h.ID, ErrConfig)
	}
	for _, r := range h.Runs {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("hold %q names a blank run: %w", h.ID, ErrConfig)
		}
	}
	return nil
}

// HoldsOf returns the active holds in store's Holder that cover runID, or none for a store with no
// Holder. A store that keeps holds and cannot read them is an error: an erasure must not go ahead
// on a guess.
func HoldsOf(ctx context.Context, store Store, runID string) ([]Hold, error) {
	h, ok := Capability[Holder](store)
	if !ok {
		return nil, nil
	}
	all, err := h.Holds(ctx)
	if err != nil {
		return nil, err
	}
	var out []Hold
	for _, x := range all {
		if x.Covers(runID) {
			out = append(out, x)
		}
	}
	return out, nil
}

// holdIDs lists the IDs of hs for a message.
func holdIDs(hs []Hold) string {
	ids := make([]string, len(hs))
	for i, h := range hs {
		ids[i] = h.ID + " (" + h.Matter + ")"
	}
	return strings.Join(ids, ", ")
}
