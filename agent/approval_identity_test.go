package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// The identity records are a contract with readers (bide view, the auditor): their JSON member names
// are pinned, and a record round-trips with an unknown member ignored.
func TestApprovalIdentityRecordsJSON(t *testing.T) {
	id := ApprovalIdentity{Format: ApprovalIdentityFormat, ToolUseID: "c1", PolicyDigest: "pd", RosterDigest: "rd", Rules: ApprovalIdentityRulesVer, Verifier: "go1.27.0",
		Policy:    IdentityPolicyRecord{EffectiveFrom: "2026-10-15T00:00:00Z", Anchors: []string{"a"}, Intermediates: []string{"i"}, EKU: "e", IDKind: "person-uri", Canonical: "c1", W: "24h0m0s", MinW: "30m0s", MaxStatusAge: "1h0m0s", ExecStatusAge: "15m0s", CAPeriod: "1h0m0s", Skew: "1m0s", AnchorGrace: "5m0s"},
		Decisions: []IdentityDecision{{Approver: "alice", Step: "s", Identifier: "id", LeafHash: "l", Chain: []string{"l", "i"}, Anchor: "a", AnchoredAt: "t", TimeSource: "log-signer clock", HeadSize: 3, Tier: TierLogclockFinal, Basis: "b", StatusTime: "s", Window: "24h0m0s", Status: []string{"h"}}}}
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{`"format":"bide.approval-identity.v2"`, `"tool_use_id":"c1"`, `"policy_digest":"pd"`, `"roster_digest":"rd"`, `"decisions":[`, `"leaf_hash":"l"`,
		`"time_source":"log-signer clock"`, `"head_size":3`, `"tier":"logclock-final"`, `"status_time":"s"`, `"effective_from"`, `"max_status_age"`, `"exec_status_age"`, `"ca_period"`, `"verifier":"go1.27.0"`} {
		if !strings.Contains(string(b), m) {
			t.Fatalf("missing %s in %s", m, b)
		}
	}
	var back ApprovalIdentity
	if err := json.Unmarshal([]byte(strings.Replace(string(b), `"rules"`, `"future":1,"rules"`, 1)), &back); err != nil || back.Decisions[0].Approver != "alice" || back.Rules != "v1" {
		t.Fatalf("%v %+v", err, back)
	}
	w := ApprovalWait{ToolUseID: "c1", IdentityBound: true, IdentityApprovers: []string{"alice"}, IdentityKinds: "k", IdentityPolicyDigest: "pd", Pause: PauseAwaitingAnchor, PauseDetail: "d"}
	wb, _ := json.Marshal(w)
	for _, m := range []string{`"identity_bound":true`, `"identity_approvers":["alice"]`, `"identity_kinds":"k"`, `"identity_policy_digest":"pd"`, `"pause":"awaiting-anchor"`, `"pause_detail":"d"`} {
		if !strings.Contains(string(wb), m) {
			t.Fatalf("missing %s in %s", m, wb)
		}
	}
	// A bare-key gate's note is what it was.
	if bb, _ := json.Marshal(ApprovalWait{ToolUseID: "c1"}); strings.Contains(string(bb), "identity") || strings.Contains(string(bb), "pause") {
		t.Fatalf("a bare-key note carries identity members: %s", bb)
	}
	if ApprovalIdentTallyStep("c 1", 0) != ApprovalTallyStep("c 1")+":ident" || ApprovalIdentTallyStep("c 1", 2) != ApprovalTallyStep("c 1")+":ident-reask:2" ||
		ApprovalIdentityStep("c1", 1) != "approval-identity:c1:1" || ApprovalStatusStep("ab") != "approval-status:ab" || ApprovalExecHoldStep("c1", 3) != "approval-exec-hold:c1:3" ||
		ApprovalIdentityDigestStep("c1", 0, "0123456789abcdef0123") != "approval-identity:c1:0:0123456789abcdef" {
		t.Fatal("step names")
	}
	recs := []Record{{Kind: StepValue, Name: ApprovalExecHoldStep("c1", 0)}, {Kind: StepValue, Name: ApprovalExecHoldStep("c2", 0)}, {Kind: StepValue, Name: ApprovalExecHoldStep("c1", 1)}}
	if identGen(recs, "c1") != 2 || identGen(recs, "c2") != 1 || identGen(recs, "c3") != 0 {
		t.Fatal("identGen")
	}
}
