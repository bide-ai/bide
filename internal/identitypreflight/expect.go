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

// undeclarable are the members of the gate's policy record that a -params file may state (or leave out).
// When the file leaves one out the stand-in approver fills a default (or a start time), so it cannot be
// compared: it is listed with the recorded value, never reported as matching. count_live (forced true in a
// declaration) and independent_time (never set) are always compared.
var undeclarable = []string{"effective_from", "w", "min_w", "routine_grace", "clock_tolerance", "allow_holds",
	"consecutive_numbers", "retained"}

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

// compareRecords returns the members that differ (naming both values), and the members that could not be
// compared with the recorded value because the declaration file leaves them out (missing).
func compareRecords(mine, gate agent.IdentityPolicyRecord, missing []string) (diffs, unverified []string) {
	a, g := asMap(mine), asMap(gate)
	skip := map[string]bool{}
	for _, k := range missing {
		if slices.Contains(undeclarable, k) {
			skip[k] = true
		}
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
