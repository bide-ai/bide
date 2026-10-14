package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/bide-ai/bide/internal/strictjson"
)

// runPrincipalStep is the journal name of the record that names a run's principal, and the
// principals of the runs above it (see RunPrincipal). A run's first drive writes it before
// run:start, so it is the first record after the journal's header.
const runPrincipalStep = "run:principal"

// RunPrincipal is what the run:principal record holds: the principal a run acts for, and the
// principals of the runs above it, root first. It holds nothing else (not the run's input), so
// evidence can prove who requested a run on its own: audit.ApprovalEvidence discloses it for an
// approval gate that excludes the requester (ApprovalPolicy.ExcludePrincipal), in place of the run's
// run:start.
//
// The run's first drive writes it, before run:start and only if it is absent, when the run has a
// principal (its Identity's OnBehalfOf or AuthorityRef) or was started inside another run's drive
// whose chain names one. Principal is the same as RunStart.Principal (empty when the run has
// none), and Ancestors the same as RunStart.Ancestors: a drive that finds a run:principal another
// drive wrote with other values is refused with ErrConfig. A run an earlier version started has none
// until a drive under this version first loads it.
type RunPrincipal struct {
	Principal Principal   `json:"principal"`
	Ancestors []Principal `json:"ancestors,omitempty"`
}

// Requesters is every requester p names: the OnBehalfOf of each ancestor, root first, then the
// run's own. An entry is empty for a run that records no OnBehalfOf. An approval policy with
// ExcludePrincipal counts none of them.
func (p RunPrincipal) Requesters() []string {
	out := make([]string, 0, len(p.Ancestors)+1)
	for _, a := range p.Ancestors {
		out = append(out, a.OnBehalfOf)
	}
	return append(out, p.Principal.OnBehalfOf)
}

// Matches reports whether p names the principal and the ancestors start does (RunStart.Principal,
// read as an empty principal when nil, and RunStart.Ancestors): the agreement a drive and the gate
// hold run:principal and run:start to.
func (p RunPrincipal) Matches(start RunStart) bool {
	var own Principal
	if start.Principal != nil {
		own = *start.Principal
	}
	return p.Principal == own && slices.Equal(p.Ancestors, start.Ancestors)
}

// IsRunPrincipal reports whether r is a run's run:principal record (see RunPrincipal).
func IsRunPrincipal(r Record) bool { return r.Kind == StepValue && r.Name == runPrincipalStep }

// RecordedPrincipal returns the run:principal record among recs (see RunPrincipal), read strictly:
// no duplicate or case-variant name at any depth (names a later version adds are allowed), so the
// record names one principal to every reader, the gate, an auditor and a verifier in another
// language alike. ok is false when recs hold none, when it does not read strictly, and when recs
// hold two that differ.
func RecordedPrincipal(recs []Record) (p RunPrincipal, ok bool) {
	for _, r := range recs {
		if !IsRunPrincipal(r) {
			continue
		}
		var got RunPrincipal
		if strictjson.Unmarshal(r.Result, &got, &strictjson.Options{AllowUnknown: true}) != nil {
			return RunPrincipal{}, false
		}
		if ok && (got.Principal != p.Principal || !slices.Equal(got.Ancestors, p.Ancestors)) {
			return RunPrincipal{}, false
		}
		p, ok = got, true
	}
	return p, ok
}

type chainKey struct{}

// contextWithChain binds the principals of the run a drive drives and of the runs above it, root
// first and the run's own last, for the runs started inside the drive (a sub-agent's, a
// delegation's, a SubRunFor run's, a plan flow's): each records them as its ancestors.
func contextWithChain(ctx context.Context, chain []Principal) context.Context {
	return context.WithValue(ctx, chainKey{}, chain)
}

// chainFrom is the chain contextWithChain bound to ctx, or nil when ctx is not a drive's.
func chainFrom(ctx context.Context) []Principal {
	c, _ := ctx.Value(chainKey{}).([]Principal)
	return c
}

// runChain is the chain a drive of a run with start binds for the runs it starts.
func runChain(start RunStart) []Principal {
	var own Principal
	if start.Principal != nil {
		own = *start.Principal
	}
	return append(slices.Clone(start.Ancestors), own)
}

// startAncestors is the ancestors a run started under ctx records: the chain of the drive that
// starts it, or none when no run above it names a principal, nor the run itself (own).
func startAncestors(ctx context.Context, own *Principal) []Principal {
	chain := chainFrom(ctx)
	if own == nil && !slices.ContainsFunc(chain, func(p Principal) bool { return p != Principal{} }) {
		return nil
	}
	return slices.Clone(chain)
}

// putPrincipal records start's run:principal in runID, a run that has no run:start yet, before
// run:start: if start names a principal or ancestors. A run:principal already there (a concurrent
// first drive's, or one written by a drive that stopped before its run:start) must name the same
// principal and ancestors, or the drive is refused with ErrConfig. wrote reports whether there was
// one to write.
func putPrincipal(ctx context.Context, j *Journal, runID string, start RunStart) (wrote bool, err error) {
	if start.Principal == nil && len(start.Ancestors) == 0 {
		return false, nil
	}
	want := RunPrincipal{Ancestors: start.Ancestors}
	if start.Principal != nil {
		want.Principal = *start.Principal
	}
	b, err := marshalJournal(want)
	if err != nil {
		return false, fmt.Errorf("encode %s (run %s): %w (%w)", runPrincipalStep, runID, err, ErrConfig)
	}
	got, inserted, err := j.putNew(ctx, runID, runPrincipalStep, Record{Kind: StepValue, Result: b})
	if err != nil {
		return false, fmt.Errorf("record %s (run %s): %w", runPrincipalStep, runID, err)
	}
	if inserted {
		return true, nil
	}
	var have RunPrincipal
	if json.Unmarshal(got.Result, &have) != nil || have.Principal != want.Principal || !slices.Equal(have.Ancestors, want.Ancestors) {
		return true, fmt.Errorf("run %s records another principal (run:principal) than this drive's; drive it with that one, or start a new run with a new run ID: %w", runID, ErrConfig)
	}
	return true, nil
}
