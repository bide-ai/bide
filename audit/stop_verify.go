package audit

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/bide-ai/bide/agent"
)

// MaxStopRecords bounds the records VerifyStops reads from the control run for its comparison with the
// trail: past it the comparison is not made and the count is a problem, so a flood of records written into
// the control run is a finding, not a hang. A deployment's stops number in the tens.
const MaxStopRecords = 20000

// StopVerifyInput is what VerifyStops checks: a stop trail, the signed heads over it, the policy the
// deployment pinned, and, when the control run is at hand, its records to compare with.
type StopVerifyInput struct {
	Trail StopTrailExport
	// Heads are signed tree heads over the trail (SignStopTrail, or the heads an AuditedEventStore
	// anchored); Verifier verifies them. Each must verify and commit to a prefix of the trail.
	Heads    []SignedTreeHead
	Verifier Verifier
	// Unanchored accepts a trail with no signed head: it is read, and nothing attests that it is complete
	// or unaltered.
	Unanchored bool
	// Now dates the check; MaxClockSkew bounds how far a head's time may be ahead of it; MaxTailAge, when
	// positive, fails a trail whose newest records have gone longer than that without a signed head.
	Now          time.Time
	MaxClockSkew time.Duration
	MaxTailAge   time.Duration
	// Options configure the fold of the trail's records (agent.WithStopMode, WithStopPolicy,
	// WithStopRoot): the policy the deployment pinned. Without a policy the records are read as a basic
	// control reads them, and nothing is verified.
	Options []agent.StopOption
	// Control are the records of the control run, in store order, to compare with the trail; nil when the
	// store is not at hand (the comparison is then not made).
	Control     []agent.StopRecord
	HaveControl bool
}

// StopReportEntry is one stop as VerifyStops reports it.
type StopReportEntry struct {
	ID           string   `json:"id"`
	Scope        string   `json:"scope"`
	Reason       string   `json:"reason,omitempty"`
	By           string   `json:"by"`
	At           string   `json:"at"`
	Signed       bool     `json:"signed"`
	Lifted       bool     `json:"lifted"`
	LiftedBy     []string `json:"lifted_by,omitempty"`
	LiftsHeld    int      `json:"lifts_held,omitempty"`
	LiftsNeeded  int      `json:"lifts_needed,omitempty"`
	Compensation string   `json:"compensation,omitempty"`
}

// StopReportRecord names a record in a comparison finding.
type StopReportRecord struct {
	Kind string `json:"kind"`
	By   string `json:"by"`
	At   string `json:"at"`
	ID   string `json:"id,omitempty"`
}

// StopReport is what VerifyStops found.
type StopReport struct {
	Records       int               `json:"records"`
	Anchored      bool              `json:"anchored"`
	Heads         int               `json:"heads"`
	HeadSize      int               `json:"head_size"`
	HeadTime      time.Time         `json:"head_time"`
	Tail          int               `json:"tail"`
	TailAge       time.Duration     `json:"tail_age,omitempty"`
	Mode          string            `json:"mode"`
	PolicyDigest  string            `json:"policy_digest,omitempty"`
	PolicySeq     int               `json:"policy_seq"`
	Stops         []StopReportEntry `json:"stops"`
	RejectedTotal int               `json:"rejected_total"`
	Rejected      []agent.Rejection `json:"rejected,omitempty"`
	ChainErr      string            `json:"chain_error,omitempty"`
	// StoreOnly are records the control run holds that the trail lacks; TrailOnly records the trail holds
	// that the control run lacks. Both are only filled when the control run was supplied.
	StoreOnly []StopReportRecord `json:"store_only,omitempty"`
	TrailOnly []StopReportRecord `json:"trail_only,omitempty"`
	// Problems are the reasons the trail is not what a deployment's stop trail should be; any makes
	// VerifyStops return ErrNotVerified.
	Problems []string `json:"problems,omitempty"`
}

func refOf(r agent.StopRecord) StopReportRecord {
	return StopReportRecord{Kind: r.Kind, By: r.By, At: r.At, ID: r.StopID}
}

// VerifyStops checks a stop trail: that its signed heads verify and commit to prefixes of it (so no
// record was deleted, changed or reordered since a head covered it), that every record decodes and
// verifies under the policy the deployment pinned and the chain of policy transitions from its root (a
// record that does not is a problem: a forgery written into the trail, or a trail from another
// deployment), that the chain can be trusted, and, with the control run, that the two hold the same
// records. It returns the stops in force as the trail shows them. A trail that does not hold together
// returns the report with an error wrapping ErrNotVerified.
func VerifyStops(in StopVerifyInput) (StopReport, error) {
	rep := StopReport{Records: len(in.Trail.Leaves), Mode: string(agent.StopBasic)}
	bad := func(format string, args ...any) { rep.Problems = append(rep.Problems, fmt.Sprintf(format, args...)) }
	if in.Trail.Format != "" && in.Trail.Format != StopTrailExportFormat {
		return rep, fmt.Errorf("audit: stop trail export format %q, want %q: %w", in.Trail.Format, StopTrailExportFormat, ErrFormat)
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	recs := make([]agent.StopRecord, 0, len(in.Trail.Leaves))
	decoded := true
	for i, leaf := range in.Trail.Leaves {
		r, err := decodeStopLeaf(leaf)
		if err != nil {
			bad("record %d: %v", i, err)
			decoded = false
			continue
		}
		recs = append(recs, r)
	}
	in.checkStopHeads(&rep, in.Trail.Leaves, recs, decoded, bad)

	if len(in.Options) > 0 {
		st, err := agent.VerifyStopRecords(recs, in.Options...)
		if err != nil {
			return rep, err
		}
		rep.Mode, rep.PolicyDigest, rep.PolicySeq = string(st.Mode), st.PolicyDigest, st.PolicySeq
		rep.RejectedTotal, rep.Rejected, rep.ChainErr = st.RejectedTotal, st.Rejected, st.ChainErr
		for _, s := range st.Stops {
			rep.Stops = append(rep.Stops, StopReportEntry{ID: s.ID, Scope: s.Scope.String(), Reason: s.Reason, By: s.By, At: s.At, Signed: s.Signed,
				Lifted: s.Lifted, LiftedBy: s.LiftedBy, LiftsHeld: s.LiftsHeld, LiftsNeeded: s.LiftsNeeded, Compensation: s.Compensation})
		}
		if st.RejectedTotal > 0 {
			bad("%d record(s) of the trail do not verify under the pinned policy and its chain (first: %s: %s): a forgery written into the trail, or a trail of another deployment", st.RejectedTotal, st.Rejected[0].Name, st.Rejected[0].Reason)
		}
		if st.ChainErr != "" {
			bad("the policy chain cannot be trusted: %s", st.ChainErr)
		}
	}
	if in.HaveControl && len(in.Control) > MaxStopRecords {
		bad("the control run holds %d records, over the %d the comparison reads: a flood of records written into the store, or a deployment that has outgrown this check", len(in.Control), MaxStopRecords)
	} else if in.HaveControl {
		storeOnly, trailOnly := agent.DiffStopRecords(in.Control, recs)
		for _, r := range storeOnly {
			rep.StoreOnly = append(rep.StoreOnly, refOf(r))
		}
		for _, r := range trailOnly {
			rep.TrailOnly = append(rep.TrailOnly, refOf(r))
		}
		if len(storeOnly) > 0 {
			bad("%d record(s) are in the control run and not in the trail: in force without evidence (agent.StopControl.Reaudit appends them), or written around the trail", len(storeOnly))
		}
		if len(trailOnly) > 0 {
			bad("%d record(s) are in the trail and not in the control run: a stop or a lift that was recorded and is not in force (a row deleted from the store, or a write that never completed)", len(trailOnly))
		}
	}
	if len(rep.Problems) > 0 {
		return rep, notVerified("audit: stops: %d problem(s), first: %s", len(rep.Problems), rep.Problems[0])
	}
	return rep, nil
}

// checkStopHeads checks the trail against its signed heads, as the notification trail's check does: every
// head must verify and commit to a prefix of the trail.
func (in StopVerifyInput) checkStopHeads(rep *StopReport, leaves [][]byte, recs []agent.StopRecord, decoded bool, bad func(string, ...any)) {
	if len(in.Heads) == 0 {
		if !in.Unanchored {
			bad("no signed head: nothing attests that the trail is complete or unaltered (give one, or allow an unanchored trail)")
		}
		return
	}
	if in.Verifier == nil {
		bad("signed heads were given without a key to verify them with")
		return
	}
	heads := slices.Clone(in.Heads)
	slices.SortStableFunc(heads, func(a, b SignedTreeHead) int {
		if c := cmp.Compare(a.Size, b.Size); c != 0 {
			return c
		}
		return cmp.Compare(a.TimestampNanos, b.TimestampNanos)
	})
	ok := true
	var newest SignedTreeHead
	for _, h := range heads {
		fail := func(format string, args ...any) {
			bad("signed head of size %d: %s", h.Size, fmt.Sprintf(format, args...))
			ok = false
		}
		switch {
		case h.Verify(in.Verifier) != nil:
			fail("does not verify: %v", h.Verify(in.Verifier))
			continue
		case h.Kind != TreeEvents:
			fail("is of the %q tree, not an event trail", h.Kind)
			continue
		case h.RunID != agent.StopTrailName:
			fail("is of the trail %q, not %q", h.RunID, agent.StopTrailName)
			continue
		}
		if err := CheckTimestamp(h.TreeHead, in.Now, in.MaxClockSkew); err != nil {
			fail("%v", err)
			continue
		}
		if h.Size > len(leaves) {
			fail("the trail has %d record(s) and the head commits to %d: records were deleted", len(leaves), h.Size)
			continue
		}
		if !bytes.Equal(merkleRoot(leaves[:h.Size]), h.Root) {
			fail("the first %d record(s) are not the ones the head commits to: a record was changed, reordered or replaced", h.Size)
			continue
		}
		newest = h
	}
	if !ok {
		return
	}
	rep.Anchored, rep.Heads = true, len(heads)
	rep.HeadSize, rep.HeadTime = newest.Size, time.Unix(0, newest.TimestampNanos).UTC()
	rep.Tail = len(leaves) - newest.Size
	if rep.Tail > 0 && decoded && newest.Size < len(recs) {
		if t, err := time.Parse(time.RFC3339Nano, recs[newest.Size].At); err == nil {
			if age := in.Now.Sub(t); age > 0 {
				rep.TailAge = age
			}
		}
		if in.MaxTailAge > 0 && rep.TailAge > in.MaxTailAge {
			bad("%d record(s) after the newest signed head (size %d) have gone unattested for %s, longer than the %s allowed: the anchoring is not keeping up", rep.Tail, newest.Size, rep.TailAge.Round(time.Second), in.MaxTailAge)
		}
	}
}
