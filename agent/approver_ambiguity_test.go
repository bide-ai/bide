package agent

import (
	"errors"
	"strings"
	"testing"
)

// Approver ids are printable ASCII (0x21 to 0x7E): letters, digits and punctuation, no space, no
// control character, nothing outside ASCII. An id outside that set could look like another one (a
// Cyrillic "о" for a Latin "o", a fullwidth spelling, a combining accent) and let one approver fill
// two seats, so it is refused with ErrConfig, naming the id.
func TestApprovalPolicy_RefusesIDsOutsidePrintableASCII(t *testing.T) {
	for name, id := range map[string]string{
		"homoglyph (Cyrillic o)": "оps", // "оps", which reads as "ops"
		"fullwidth":              "ａｌｉｃｅ",
		"accent (NFC)":           "café",
		"accent (NFD)":           "café",
		"sharp s":                "straße",
		"space":                  "al ice",
		"leading space":          " ops",
		"tab":                    "ops\t",
		"newline":                "ops\n",
		"DEL":                    "ops\x7f",
		"NUL":                    "ops\x00",
		"invalid UTF-8":          "bob\xff",
		"zero-width space":       "ops​",
	} {
		approvers := []string{"ops", "finance", id}
		err := ApprovalPolicy{Need: 1, Approvers: approvers}.Validate()
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: Validate(%q) = %v, want ErrConfig", name, approvers, err)
			continue
		}
		if !strings.Contains(err.Error(), "printable ASCII") {
			t.Errorf("%s: error %q does not say ids are printable ASCII", name, err)
		}
	}
}

// The homoglyph case on its own: "ops" in Latin letters and "оps" with a Cyrillic "о" are never
// both accepted, though no case folding or Unicode normalization equates them.
func TestApprovalPolicy_RefusesHomoglyph(t *testing.T) {
	err := ApprovalPolicy{Need: 2, Approvers: []string{"ops", "оps"}}.Validate()
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("Validate = %v, want ErrConfig", err)
	}
}

// Two ids that differ only by ASCII case read as one approver, who could fill two seats: refused
// with ErrConfig, naming both.
func TestApprovalPolicy_RefusesIDsThatDifferOnlyByCase(t *testing.T) {
	for _, pair := range [][2]string{
		{"alice", "Alice"},
		{"bob", "BOB"},
		{"Ops@Example.com", "ops@example.com"},
		{"carol", "CAROL"},
	} {
		approvers := []string{pair[0], "dave", pair[1]}
		err := ApprovalPolicy{Need: 1, Approvers: approvers}.Validate()
		if !errors.Is(err, ErrConfig) {
			t.Errorf("Validate(%q) = %v, want ErrConfig", approvers, err)
			continue
		}
		if !strings.Contains(err.Error(), pair[0]) || !strings.Contains(err.Error(), pair[1]) || !strings.Contains(err.Error(), "case") {
			t.Errorf("error %q does not name both %q and %q as differing only by case", err, pair[0], pair[1])
		}
	}
}

// An empty id is refused.
func TestApprovalPolicy_RefusesEmptyID(t *testing.T) {
	if err := (ApprovalPolicy{Need: 1, Approvers: []string{"alice", ""}}).Validate(); !errors.Is(err, ErrConfig) {
		t.Fatalf("Validate = %v, want ErrConfig", err)
	}
}

// Ids in printable ASCII that differ other than by case pass, emails and URIs included.
func TestApprovalPolicy_DistinctASCIIIDsPass(t *testing.T) {
	for _, approvers := range [][]string{
		{"alice", "bob"},
		{"alice", "alice2"},
		{"alice", "al1ce"},
		{"ops@example.com", "finance@example.com", "risk.team+oncall@example.co.uk"},
		{"spiffe://example.org/approver/ops", "urn:bide:approver:finance"},
		{"!", "~", "a_b-c.d"},
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
