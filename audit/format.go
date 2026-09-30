package audit

import (
	"encoding/json"
	"errors"
	"fmt"
)

// The format tags a top-level proof artifact carries in its "format" field. Each names the
// artifact's JSON layout, so a verifier refuses a layout it does not read with ErrFormat instead of
// misreading it or failing on an unknown field. The artifacts introduced in v2 of their layout
// were untagged before it (v1), and v1 spelled the inclusion and consistency proof fields in Go
// case ("Index", "Size", "Path", "First", "Salt"); every name is snake_case from v2 on.
const (
	// ProofFormat is the format of a ProofBundle.
	ProofFormat = "bide.audit.proof.v2"
	// AbsenceFormat is the format of an AbsenceBundle.
	AbsenceFormat = "bide.audit.absence.v2"
	// RunCertificateFormat is the format of a RunCertificate.
	RunCertificateFormat = "bide.audit.runcert.v2"
	// CurrentGrantFormat is the format of a CurrentGrantProof.
	CurrentGrantFormat = "bide.audit.current-grant.v2"
	// EventInclusionFormat is the format of an EventInclusion.
	EventInclusionFormat = "bide.audit.event-inclusion.v2"
)

// ErrFormat reports an artifact whose "format" is missing or is not the one this version of the
// package reads: an artifact produced by an older (or newer) release. Re-create the artifact with
// this release's producer (ProveRecord, Evidence, CertifyRun, ...) from the journal.
var ErrFormat = errors.New("audit: unsupported artifact format")

// formatted is a top-level artifact with a "format" field. UnmarshalStrict checks the field before
// anything else, so an artifact of another format fails with ErrFormat, not with a decode error
// about a field the other format names differently.
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
