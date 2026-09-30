package agent

import (
	"errors"
	"strings"
	"testing"
)

// Approver ids are compared as exact bytes, so a policy listing two ids a person (or a key lookup
// that folds case or normalizes Unicode) reads as the same approver would let one approver fill two
// seats. Such a policy is ambiguous: Validate refuses it with ErrConfig naming both ids.
func TestApprovalPolicy_RefusesIDsThatDifferOnlyByCaseOrNormalization(t *testing.T) {
	for name, pair := range map[string][2]string{
		"case":                 {"alice", "Alice"},
		"upper case":           {"bob", "BOB"},
		"NFC and NFD":          {"café", "café"},
		"fullwidth (NFKC)":     {"alice", "ａｌｉｃｅ"},
		"ligature (NFKC)":      {"office", "oﬃce"},
		"sharp s (case fold)":  {"strasse", "straße"},
		"final sigma (fold)":   {"σοφος", "σοφοσ"},
		"case and NFD":         {"Café", "café"},
		"third id is the echo": {"carol", "CAROL"},
		// Canonically equivalent only once the iota subscript is ordered after the acute accent,
		// which case folding alone does not do: the ids are decomposed before folding.
		"ypogegrammeni order": {"\u1f84", "\u1f80\u0301"},
	} {
		approvers := []string{pair[0], "dave", pair[1]}
		err := ApprovalPolicy{Need: 1, Approvers: approvers}.Validate()
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: Validate(%q) = %v, want ErrConfig", name, approvers, err)
			continue
		}
		if !strings.Contains(err.Error(), pair[0]) || !strings.Contains(err.Error(), pair[1]) {
			t.Errorf("%s: error %q does not name both %q and %q", name, err, pair[0], pair[1])
		}
	}
}

// An approver id that is not valid UTF-8 has no normal form to compare, and the audit trail
// encodes it lossily, so it is refused outright.
func TestApprovalPolicy_RefusesInvalidUTF8ID(t *testing.T) {
	err := ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob\xff"}}.Validate()
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("Validate = %v, want ErrConfig", err)
	}
}

// Distinct approvers stay distinct: ids that differ other than by case or normalization pass.
func TestApprovalPolicy_DistinctIDsPass(t *testing.T) {
	for _, approvers := range [][]string{
		{"alice", "bob"},
		{"alice", "alice2"},
		{"café", "cafe"},
		{"alice", "al1ce"},
	} {
		if err := (ApprovalPolicy{Need: 1, Approvers: approvers}).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", approvers, err)
		}
	}
}

// An id listed twice is reported as a repeat, not as two spellings of one approver.
func TestApprovalPolicy_ExactRepeatIsReportedAsTwice(t *testing.T) {
	err := ApprovalPolicy{Need: 1, Approvers: []string{"alice", "alice"}}.Validate()
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("Validate = %v, want ErrConfig saying the id is listed twice", err)
	}
}
