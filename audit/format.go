package audit

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// The format tags an artifact carries in its "format" field. Each names the artifact's JSON layout
// and what its proofs commit to, so a verifier refuses a layout it does not read with ErrFormat
// instead of misreading it or failing on a field it does not know.
//
// Before 1.0 a verifier reads only the current format of each artifact; an artifact an older
// release made is refused with ErrFormat, and is re-created from the journal with this release's
// producer. From 1.0 on, a verifier reads the current format and the one before it.
const (
	// ProofFormat is the format of a ProofBundle. v3 carries the record as the bytes the journal
	// stores (record_bytes) rather than a decoded record.
	ProofFormat = "bide.audit.proof.v3"
	// AbsenceFormat is the format of an AbsenceBundle. v3 carries a bide.audit.sth.v5 head.
	AbsenceFormat = "bide.audit.absence.v3"
	// RunCertificateFormat is the format of a RunCertificate. v3 carries bide.audit.sth.v5 heads
	// and bide.audit.proof.v3 bundles.
	RunCertificateFormat = "bide.audit.runcert.v3"
	// CurrentGrantFormat is the format of a CurrentGrantProof. v3 carries a bide.audit.proof.v3
	// leaf and bide.audit.grant.v2 grants.
	CurrentGrantFormat = "bide.audit.current-grant.v3"
	// EventInclusionFormat is the format of an EventInclusion. v3 proves a
	// bide.audit.event-leaf.v3 leaf, whose event JSON is snake_case.
	EventInclusionFormat = "bide.audit.event-inclusion.v3"
	// STHFormat is the format of a SignedTreeHead, and the tag of the bytes it signs. v5 signs
	// the scheme (Alg) with the head.
	STHFormat = "bide.audit.sth.v5"
	// AnchorEntryFormat is the format of an AnchorEntry.
	AnchorEntryFormat = "bide.audit.anchor-entry.v1"
)

// The errors a verifier returns. A verifier returns nil when the artifact verifies. Otherwise its
// error wraps exactly one of:
//
//   - ErrNotVerified: the artifact was read and understood, and it does not hold (a signature that
//     does not verify, a proof against another root, a record of the wrong kind for its role);
//   - ErrFormat: the artifact, or one it carries, is not of the format this version reads;
//   - ErrMalformed: the artifact cannot be read as what it claims to be (JSON that does not read
//     strictly, record bytes that do not decode, a key that is not of its scheme's form).
//
// ErrFormat wraps ErrMalformed, and ErrMalformed wraps agent.ErrProtocol. A caller's mistake (no
// verifier, an invalid expected policy) wraps agent.ErrConfig instead. Only nil means verified.
var (
	// ErrMalformed reports an artifact that cannot be read as what it claims to be.
	ErrMalformed = fmt.Errorf("audit: malformed artifact: %w", agent.ErrProtocol)
	// ErrFormat reports an artifact whose "format" is missing or is not the one this version of
	// the package reads: an artifact produced by an older (or newer) release. Re-create the
	// artifact with this release's producer (ProveRecord, Evidence, CertifyRun, ...) from the
	// journal.
	ErrFormat = fmt.Errorf("audit: unsupported artifact format: %w", ErrMalformed)
	// ErrNotVerified reports an artifact that was read and understood and does not hold.
	ErrNotVerified = errors.New("audit: not verified")
)

// notVerified returns an error wrapping ErrNotVerified with the reason.
func notVerified(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrNotVerified)
}

// formatted is an artifact with a "format" field. UnmarshalStrict checks the field before the
// rest of the artifact, wherever the artifact sits in the input, so an artifact of another format
// fails with ErrFormat, not with a decode error about a field the other format names differently.
type formatted interface {
	// artifactFormat returns the artifact's name, for errors, and the format this package reads.
	artifactFormat() (name, format string)
}

func (ProofBundle) artifactFormat() (string, string)   { return "proof bundle", ProofFormat }
func (AbsenceBundle) artifactFormat() (string, string) { return "absence bundle", AbsenceFormat }
func (RunCertificate) artifactFormat() (string, string) {
	return "run certificate", RunCertificateFormat
}
func (CurrentGrantProof) artifactFormat() (string, string) {
	return "current-grant proof", CurrentGrantFormat
}
func (EventInclusion) artifactFormat() (string, string) {
	return "event inclusion proof", EventInclusionFormat
}
func (EvidencePackage) artifactFormat() (string, string) { return "evidence package", EvidenceFormat }
func (SignedTreeHead) artifactFormat() (string, string)  { return "signed tree head", STHFormat }
func (AnchorEntry) artifactFormat() (string, string)     { return "anchor entry", AnchorEntryFormat }

// checkFormat returns an ErrFormat error unless got is want.
func checkFormat(name, got, want string) error {
	switch got {
	case want:
		return nil
	case "":
		return fmt.Errorf("the %s has no format: it predates format %q, the one this version reads (re-create it from the journal): %w", name, want, ErrFormat)
	default:
		return fmt.Errorf("the %s has format %q; this version reads %q: %w", name, got, want, ErrFormat)
	}
}

// formatOf returns an ErrFormat error unless got is the format f's type reads.
func formatOf(f formatted, got string) error {
	name, want := f.artifactFormat()
	return checkFormat(name, got, want)
}

// checkDataFormat checks the top-level "format" member of data, an artifact of type f, before the
// rest of data is decoded. Input that is not a JSON object is left to the strict decoder to reject.
func checkDataFormat(data []byte, f formatted) error {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return nil
	}
	name, want := f.artifactFormat()
	var got string
	if raw, ok := top["format"]; ok {
		if json.Unmarshal(raw, &got) != nil {
			return fmt.Errorf("the %s has a format that is not a string: %w", name, ErrFormat)
		}
	}
	return checkFormat(name, got, want)
}
