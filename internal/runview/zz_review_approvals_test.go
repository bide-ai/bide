package runview

// Adversarial review of #244 (decision times). The test reproduces a finding and fails on the
// reviewed branch.

import "testing"

// TestReview_CardShowsUnsignedTime: without approver keys the card takes the approver's FIRST
// journaled decision with the card's outcome, valid or not. Anyone who can call SubmitDecision
// can journal a copy of the approver's signature under another decided_at before the real one
// lands; it never counts (ReasonBadSig), but the card shows its time, with the "decided" label
// whose tooltip says the time is inside the signature and cannot change after signing, and the
// sanity flags are computed on it. Here the forged time hides the "decided before asked" flag the
// real decision would get. The verdict path (keys given) is not affected.
func TestReview_CardShowsUnsignedTime(t *testing.T) {
	g := Gate{ToolUseID: "c1", Need: 1, Approvers: []string{"ops"}, ApprovedBy: []string{"ops"},
		AskedAt: "2026-10-13T12:00:00.000Z",
		Decisions: []Decision{
			// a copy of ops's signature journaled first under a time ops never signed
			{Approver: "ops", Approved: true, Signed: true, DecidedAt: "2026-10-13T12:00:30.000Z", RecordedAt: "2026-10-13T12:00:31.000Z"},
			// ops's real decision, the one the gate counted
			{Approver: "ops", Approved: true, Signed: true, DecidedAt: "2026-10-13T11:00:00.000Z", RecordedAt: "2026-10-13T12:00:40.000Z"},
		}}
	c := cards(g, nil)[0]
	if c.DecidedAt == "2026-10-13 12:00:30.000" {
		t.Fatalf("the card shows decided %q, a time ops never signed (flags %q), with no sign it is unverified or that ops has %d signed decisions on record",
			c.DecidedAt, c.Flags, len(g.Decisions))
	}
}
