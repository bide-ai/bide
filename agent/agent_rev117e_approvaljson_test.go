package agent

import (
	"encoding/json"
	"testing"
)

// DecodeStoredRecord promises: "Fields this version does not know still decode, so a journal a
// newer version wrote stays readable." Record decoding is lenient at the top level, but the
// approval member is decoded strictly (ApprovalPolicy.UnmarshalJSON), so one unknown member inside
// "approval" makes the whole record, and so the run's History, unreadable (ErrStorage).
func TestRev117e_RecordApprovalUnknownMemberBreaksForwardCompat(t *testing.T) {
	top := `{"name":"tool:c1","kind":"tool_result","tool_use_id":"c1","result":"1","future_top":1}`
	if _, err := DecodeStoredRecord("r", "tool:c1", []byte(top)); err != nil {
		t.Fatalf("baseline: an unknown top-level field must decode: %v", err)
	}
	nested := `{"name":"tool:c1","kind":"tool_result","tool_use_id":"c1","result":"1","approval":{"need":1,"approvers":["a"],"expires_unix":5}}`
	if _, err := DecodeStoredRecord("r", "tool:c1", []byte(nested)); err != nil {
		t.Fatalf("a record whose approval carries a member a newer version added does not decode: %v", err)
	}
}

// Round trip of both approval forms through a Record, as a store does.
func TestRev117e_RecordApprovalRoundTrip(t *testing.T) {
	for _, p := range []*ApprovalPolicy{SingleApproval(), {Need: 2, Approvers: []string{"a", "b"}}} {
		b, err := json.Marshal(Record{Name: "tool:c1", Kind: StepToolResult, Approval: p})
		if err != nil {
			t.Fatal(err)
		}
		r, err := DecodeStoredRecord("r", "tool:c1", b)
		if err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if r.Approval.single() != p.single() || r.Approval.Need != p.Need || len(r.Approval.Approvers) != len(p.Approvers) {
			t.Fatalf("round trip %s gave %+v", b, *r.Approval)
		}
	}
}
