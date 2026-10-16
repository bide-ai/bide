package agent

import (
	"encoding/json"
	"errors"
	"testing"
)

// Pins what the approver identity design (docs/design/approver-identity.md) relies on for a rolling
// deploy: a decision record read by a binary that does not know a new field ignores it (the decision is
// then verified, and counted or not, by the rules that binary has), while a recorded tally is read
// strictly, so a field added to the tally would make an older binary fail the read with ErrStorage
// instead of pausing. New identity data therefore goes in records of its own, never in the tally.
func TestUnknownFieldsInADecisionRecordAreTolerated(t *testing.T) {
	rec := Record{Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
		Approver: "a", ApproverAlg: "x", Signature: []byte("s"), DecidedAt: "2026-10-13T12:14:03.123Z"}}
	b, err := JournalEntry("approval:c1:a:00", rec)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["approver_future"] = "AAAA"
	nb, _ := json.Marshal(m)
	got, err := DecodeStoredRecord("r", "approval:c1:a:00", nb)
	if err != nil || got.Approver() != "a" {
		t.Fatalf("a decision record with an unknown field: %v (%q)", err, got.Approver())
	}
}

func TestTallyDecodingIsStrict(t *testing.T) {
	_, err := decodeTally("r", Record{Name: "approval-tally:c1", Result: []byte(`{"need":1,"approvers":["a"],"approved":1,"denied":0,"identity_env":"abc"}`)})
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("a tally with an unknown field decodes (%v); the design's placement of identity data must be revisited", err)
	}
}

func TestApprovalWaitToleratesNewFields(t *testing.T) {
	var w ApprovalWait
	if err := json.Unmarshal([]byte(`{"tool_use_id":"c1","tool_name":"x","identity_kinds":"abc"}`), &w); err != nil || w.ToolUseID != "c1" {
		t.Fatalf("the pause note with a new field: %v", err)
	}
}
