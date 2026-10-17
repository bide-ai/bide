package audit

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// ApproverIDs returns the approver ids of the policy's identity map, sorted.
func (c *CertIdentity) ApproverIDs() []string {
	ids := make([]string, 0, len(c.identity))
	for id := range c.identity {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// TrustAnchors returns the policy's trust anchor certificates, ordered by key.
func (c *CertIdentity) TrustAnchors() []*x509.Certificate {
	keys := make([]SPKIHash, 0, len(c.anchorBy))
	for k := range c.anchorBy {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, cmpHash)
	out := make([]*x509.Certificate, len(keys))
	for i, k := range keys {
		out[i] = c.anchorBy[k]
	}
	return out
}

// statusDeclFile is the JSON form of the CA timings a gate declares (the CertApprover and StatusParams
// fields the refresher schedules by), for a refresher run outside a gate (bide identity fetch-crl). The
// values must be the ones the gate declares: the file is a copy of the declaration, not a second source.
// Durations are Go duration strings; issuer keys are lowercase hex SPKI hashes.
type statusDeclFile struct {
	CAPeriod          string `json:"ca_period"`
	MaxStatusAge      string `json:"max_status_age"`
	ExecStatusAge     string `json:"exec_status_age"`
	ThisUpdateSkew    string `json:"this_update_skew"`
	CoverageTolerance string `json:"coverage_tolerance"`
	// The members below are optional and reach the gate's policy digest (they are the members of the policy
	// record of the same names): with all of them present StatusDecl.Approver builds an approver whose
	// PolicyDigest is the gate's for the same declaration. A file without them parses and behaves as before.
	W                  string                      `json:"w"`
	MinW               string                      `json:"min_w"`
	RoutineGrace       string                      `json:"routine_grace"`
	ClockTolerance     string                      `json:"clock_tolerance"`
	AllowHolds         *bool                       `json:"allow_holds"`
	ConsecutiveNumbers *bool                       `json:"consecutive_numbers"`
	Retained           *bool                       `json:"retained"`
	EffectiveFrom      string                      `json:"effective_from"`
	Issuers            map[string]statusDeclIssuer `json:"issuers"`
}

type statusDeclIssuer struct {
	CAPeriod       string `json:"ca_period"`
	MaxStatusAge   string `json:"max_status_age"`
	ExecStatusAge  string `json:"exec_status_age"`
	CAOverlap      string `json:"ca_overlap"`
	ThisUpdateSkew string `json:"this_update_skew"`
	DeltaPeriod    string `json:"delta_period"`
}

// StatusDecl is the declared CA timings read from a file.
type StatusDecl struct {
	Params           StatusParams
	ExecStatusAge    time.Duration
	ExecStatusAgeFor map[SPKIHash]time.Duration
	CAOverlapFor     map[SPKIHash]time.Duration
	DeltaPeriodFor   map[SPKIHash]time.Duration
	// ClockTolerance and EffectiveFrom are the gate's CertApprover.ClockTolerance and PolicyEffectiveFrom, when
	// the file states them.
	ClockTolerance time.Duration
	EffectiveFrom  time.Time
	stated         map[string]bool
}

// digestMembers are the optional members of the declaration file that the policy digest covers, by their
// names in the file and in the policy record.
var digestMembers = []string{"allow_holds", "clock_tolerance", "consecutive_numbers", "coverage_tolerance", "effective_from", "min_w", "retained", "routine_grace", "w"}

// MissingForDigest returns, sorted, the members of the declaration the policy digest covers and the file
// does not state. With none missing, Approver builds an approver whose PolicyDigest equals the gate's (when the
// values are the gate's); with some, the digest still reflects the file but cannot be compared with the
// gate's, because the missing members take defaults or a stand-in start time.
func (d StatusDecl) MissingForDigest() []string {
	var out []string
	for _, m := range digestMembers {
		if !d.stated[m] {
			out = append(out, m)
		}
	}
	return out
}

// ParseStatusDecl reads the declaration file. A per-issuer exec_status_age makes that CA a declared
// tier and then needs ca_period, ca_overlap and max_status_age beside it (zero overlap is written
// "0s", never left out), as the gate's own validation requires.
func ParseStatusDecl(name string, b []byte) (StatusDecl, error) {
	bad := func(format string, args ...any) (StatusDecl, error) {
		return StatusDecl{}, fmt.Errorf("audit: status declaration %s: %s: %w", name, fmt.Sprintf(format, args...), agent.ErrConfig)
	}
	var f statusDeclFile
	if err := UnmarshalStrict(b, &f); err != nil {
		return bad("%v", err)
	}
	dur := func(field, s string) (time.Duration, bool, error) {
		if s == "" {
			return 0, false, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, false, fmt.Errorf("%s: %v", field, err)
		}
		return d, true, nil
	}
	d := StatusDecl{stated: map[string]bool{}}
	var err error
	for _, x := range []struct {
		field, s string
		into     *time.Duration
	}{{"ca_period", f.CAPeriod, &d.Params.CAPeriod}, {"max_status_age", f.MaxStatusAge, &d.Params.MaxStatusAge},
		{"exec_status_age", f.ExecStatusAge, &d.ExecStatusAge}, {"this_update_skew", f.ThisUpdateSkew, &d.Params.ThisUpdateSkew},
		{"coverage_tolerance", f.CoverageTolerance, &d.Params.CoverageTolerance}, {"w", f.W, &d.Params.W}, {"min_w", f.MinW, &d.Params.MinW},
		{"routine_grace", f.RoutineGrace, &d.Params.RoutineGrace}, {"clock_tolerance", f.ClockTolerance, &d.ClockTolerance}} {
		var ok bool
		if *x.into, ok, err = dur(x.field, x.s); err != nil {
			return bad("%v", err)
		}
		d.stated[x.field] = ok
	}
	for _, x := range []struct {
		field string
		v     *bool
		into  *bool
	}{{"allow_holds", f.AllowHolds, &d.Params.AllowHolds}, {"consecutive_numbers", f.ConsecutiveNumbers, &d.Params.ConsecutiveNumbers},
		{"retained", f.Retained, &d.Params.Retained}} {
		if x.v != nil {
			*x.into, d.stated[x.field] = *x.v, true
		}
	}
	if f.EffectiveFrom != "" {
		if d.EffectiveFrom, err = time.Parse(time.RFC3339, f.EffectiveFrom); err != nil {
			return bad("effective_from: %v", err)
		}
		d.stated["effective_from"] = true
	}
	d.Params.CountLive = true
	d.Params.CAPeriodFor, d.Params.MaxStatusAgeFor, d.Params.ThisUpdateSkewFor = map[SPKIHash]time.Duration{}, map[SPKIHash]time.Duration{}, map[SPKIHash]time.Duration{}
	d.ExecStatusAgeFor, d.CAOverlapFor, d.DeltaPeriodFor = map[SPKIHash]time.Duration{}, map[SPKIHash]time.Duration{}, map[SPKIHash]time.Duration{}
	for k, v := range f.Issuers {
		raw, derr := hex.DecodeString(k)
		if derr != nil || len(raw) != len(SPKIHash{}) || k != strings.ToLower(k) {
			return bad("issuers key %q is not a lowercase hex SHA-256", k)
		}
		h := SPKIHash(raw)
		for _, x := range []struct {
			field, s string
			m        map[SPKIHash]time.Duration
		}{{"ca_period", v.CAPeriod, d.Params.CAPeriodFor}, {"max_status_age", v.MaxStatusAge, d.Params.MaxStatusAgeFor},
			{"exec_status_age", v.ExecStatusAge, d.ExecStatusAgeFor}, {"ca_overlap", v.CAOverlap, d.CAOverlapFor},
			{"this_update_skew", v.ThisUpdateSkew, d.Params.ThisUpdateSkewFor}, {"delta_period", v.DeltaPeriod, d.DeltaPeriodFor}} {
			dd, ok, derr := dur(x.field, x.s)
			if derr != nil {
				return bad("issuer %s: %v", k[:8], derr)
			}
			if ok {
				x.m[h] = dd
			}
		}
	}
	return d, nil
}

// Approver builds the CertApprover the refresher reads its declared values from, for a refresher run
// outside a gate. It names the policy's first approver id and takes the file's effective_from as the date the policy took
// effect, or now when the file has none (a refresher never reads it, but the policy digest does).
func (d StatusDecl) Approver(policy *CertIdentity, snap *StatusSnapshot, now func() time.Time) (*CertApprover, error) {
	ids := policy.ApproverIDs()
	if len(ids) == 0 {
		return nil, fmt.Errorf("audit: status declaration: the policy names no approver: %w", agent.ErrConfig)
	}
	effective := now()
	if !d.EffectiveFrom.IsZero() {
		effective = d.EffectiveFrom
	}
	return &CertApprover{ApproverID: ids[0], Policy: policy, Status: snap, Now: now, PolicyEffectiveFrom: effective, ClockTolerance: d.ClockTolerance,
		StatusParams: d.Params, ExecStatusAge: d.ExecStatusAge, ExecStatusAgeFor: d.ExecStatusAgeFor, CAOverlapFor: d.CAOverlapFor,
		StatusArchive: noStatusArchive{}}, nil
}

// noStatusArchive accepts and drops: the approver Approver returns serves no gate, so no journal records
// a CRL by hash, and its snapshot may take CRLs of any size the limits allow.
type noStatusArchive struct{}

func (noStatusArchive) PutStatus(context.Context, string, []byte) error { return nil }
