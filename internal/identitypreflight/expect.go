package identitypreflight

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// notDeclared are the members of the gate's policy record that the policy file and the -params file cannot
// carry, so a pre-flight cannot compare them (the stand-in approver fills defaults, or a start time). They
// are listed with the recorded value, never reported as matching.
var notDeclared = []string{"effective_from", "w", "min_w", "routine_grace", "clock_tolerance", "allow_holds",
	"consecutive_numbers", "count_live", "retained", "independent_time"}

// readRecord reads the gate's recorded policy from path: an ApprovalIdentity record (the "policy" member of
// the journal's identity record), or the IdentityPolicyRecord alone. Unknown members are refused, so a
// record from another bide version is not silently compared on the members this one knows.
func readRecord(path string) (agent.IdentityPolicyRecord, error) {
	var rec agent.IdentityPolicyRecord
	b, err := os.ReadFile(path)
	if err != nil {
		return rec, &IOError{Err: fmt.Errorf("-expect: %w", err)}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return rec, fmt.Errorf("-expect %s is not a JSON object: %v", path, err)
	}
	if p, ok := top["policy"]; ok {
		b = p
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return rec, fmt.Errorf("-expect %s is not an identity policy record: %v", path, err)
	}
	if rec.EKU == "" || rec.CAPeriod == "" {
		return rec, fmt.Errorf("-expect %s holds no policy record (no eku or ca_period)", path)
	}
	return rec, nil
}

func asMap(rec agent.IdentityPolicyRecord) map[string]any {
	b, _ := json.Marshal(rec)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// compareRecords returns, for the members both sides can declare, the ones that differ (naming both
// values), and the members that could not be compared with the recorded value.
func compareRecords(mine, gate agent.IdentityPolicyRecord) (diffs, unverified []string) {
	a, g := asMap(mine), asMap(gate)
	skip := map[string]bool{}
	for _, k := range notDeclared {
		skip[k] = true
	}
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range g {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	show := func(m map[string]any, k string) string {
		v, ok := m[k]
		if !ok {
			return "(absent)"
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	for _, k := range names {
		if skip[k] {
			if v, ok := g[k]; ok && !isZero(v) {
				unverified = append(unverified, fmt.Sprintf("%s (recorded %s)", k, show(g, k)))
			}
			continue
		}
		if !reflect.DeepEqual(a[k], g[k]) {
			diffs = append(diffs, fmt.Sprintf("%s: policy and -params say %s, the gate recorded %s", k, show(a, k), show(g, k)))
		}
	}
	slices.Sort(unverified)
	return diffs, unverified
}

func isZero(v any) bool {
	return v == nil || v == false || v == "" || v == float64(0)
}

// standIn is the record of the approver the pre-flight builds from the policy file and -params, without the
// stand-in start time.
func standIn(ap *audit.CertApprover) agent.IdentityPolicyRecord {
	return ap.DescribeIdentity()
}
