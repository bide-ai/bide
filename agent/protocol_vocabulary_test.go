package agent

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The shared vocabulary of the claim code and model 1 (docs/design/formal-models.md, section
// 6.2): every journal key the claim protocol writes or reads, and every record kind it stores
// under them, has a kind in the spec, declared in the vocabulary block of
// spec/tla/claims/Claims.tla. The chain: every engine write uses a key constructor
// (TestEngineKeys_WritesUseConstructors); every claim-code constructor is in claimKeyKinds (checked
// against keyConstructors below); every kind in the tables is in the spec block, and back.

// stepNameKey stands for the key of a Step's result, which is the step's name itself, in
// claimKeyKinds.
const stepNameKey = "(the step name)"

// claimKeyKinds maps each key constructor the claim code writes through to the spec's kind of
// record stored under the keys it builds.
var claimKeyKinds = map[string]string{
	"toolAttemptStep":  "marker",
	"stepAttemptStep":  "marker",
	"retryAttemptStep": "retry_marker",
	"nextAttemptStep":  "retry_marker",
	"notStartedStep":   "not_started",
	"ToolResultStep":   "result_tool",
	stepNameKey:        "result_step",
}

// claimRecordKinds maps each record kind the claim code stores to the spec kinds of the keys it
// is stored under.
var claimRecordKinds = map[StepKind][]string{
	StepAttempt:    {"marker", "retry_marker"},
	StepNotStarted: {"not_started"},
	StepToolResult: {"result_tool"},
	StepValue:      {"result_step"},
}

// claimsSpec is model 1's spec, relative to this package.
const claimsSpec = "../spec/tla/claims/Claims.tla"

var recordKindsRE = regexp.MustCompile(`^RecordKinds\s*==\s*\{(.*)\}\s*$`)

// specRecordKinds reads RecordKinds from the vocabulary block of the spec.
func specRecordKinds(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(claimsSpec)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	in, found, blocks := false, 0, 0
	for _, line := range strings.Split(string(data), "\n") {
		switch strings.TrimSpace(line) {
		case `\* vocabulary: begin`:
			in = true
			blocks++
			continue
		case `\* vocabulary: end`:
			in = false
			continue
		}
		if !in {
			continue
		}
		if m := recordKindsRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			found++
			for _, f := range strings.Split(m[1], ",") {
				k, err := strconv.Unquote(strings.TrimSpace(f))
				if err != nil {
					t.Fatalf("%s: RecordKinds element %q is not a string: %v", claimsSpec, f, err)
				}
				kinds = append(kinds, k)
			}
		}
	}
	if blocks != 1 || in || found != 1 {
		t.Fatalf("%s: want one closed vocabulary block holding one RecordKinds line; got %d blocks, %d RecordKinds lines, open=%v", claimsSpec, blocks, found, in)
	}
	slices.Sort(kinds)
	return kinds
}

func sortedSet(xs []string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return slices.Compact(out)
}

// claimKey is a claim-protocol key parsed back into the parts the model names: its kind, the
// first attempt's marker key of the effect (base), the attempt number, and the claim id of a
// not-started record. It is the key parser the trace emitter of milestone M3 is to use.
type claimKey struct {
	kind  string
	base  string // for markers and not-started records: the first attempt's marker key
	gen   int
	claim string // for not-started records
	call  string // for results: the encoded tool-use id or the step name
}

// parseClaimKey parses a key the claim protocol writes, or reports false for any other key.
func parseClaimKey(key string) (claimKey, bool) {
	if rest, ok := strings.CutPrefix(key, notStartedPrefix); ok {
		claim, marker, ok := strings.Cut(rest, ":")
		if !ok || claim == "" {
			return claimKey{}, false
		}
		m, ok := parseClaimKey(marker)
		if !ok || (m.kind != "marker" && m.kind != "retry_marker") {
			return claimKey{}, false
		}
		return claimKey{kind: "not_started", base: m.base, gen: m.gen, claim: claim}, true
	}
	if rest, ok := strings.CutPrefix(key, retryAttemptPrefix); ok {
		n, tail, ok := strings.Cut(rest, ":")
		gen, err := strconv.Atoi(n)
		if !ok || err != nil || gen < 1 || strconv.Itoa(gen) != n {
			return claimKey{}, false
		}
		base, ok := parseClaimKey("attempt:" + tail)
		if !ok || base.kind != "marker" {
			return claimKey{}, false
		}
		return claimKey{kind: "retry_marker", base: base.base, gen: gen}, true
	}
	if strings.HasPrefix(key, "attempt:tool:") || strings.HasPrefix(key, "attempt:step:") {
		return claimKey{kind: "marker", base: key}, true
	}
	if strings.HasPrefix(key, "attempt:") {
		return claimKey{}, false
	}
	if id, ok := strings.CutPrefix(key, "tool:"); ok {
		return claimKey{kind: "result_tool", call: id}, true
	}
	if checkStepName("Step", key) == nil || planNodeStep(key) {
		return claimKey{kind: "result_step", call: key}, true
	}
	return claimKey{}, false
}

func TestProtocolVocabulary(t *testing.T) {
	spec := specRecordKinds(t)

	// The tables and the spec block name the same kinds, in both directions.
	var fromKeys []string
	for _, k := range claimKeyKinds {
		fromKeys = append(fromKeys, k)
	}
	if got := sortedSet(fromKeys); !slices.Equal(got, spec) {
		t.Errorf("key constructors map to kinds %q; %s declares RecordKinds %q", got, claimsSpec, spec)
	}
	var fromRecs []string
	for _, ks := range claimRecordKinds {
		fromRecs = append(fromRecs, ks...)
	}
	if got := sortedSet(fromRecs); !slices.Equal(got, spec) {
		t.Errorf("record kinds map to kinds %q; %s declares RecordKinds %q", got, claimsSpec, spec)
	}

	// Every constructor of an attempt: key is in the table, and every constructor in the table
	// (except the step name) is a real constructor.
	for name, build := range keyConstructors {
		if strings.HasPrefix(build("x"), "attempt:") {
			if _, ok := claimKeyKinds[name]; !ok {
				t.Errorf("key constructor %s builds attempt: keys but has no kind in claimKeyKinds", name)
			}
		}
	}
	for name := range claimKeyKinds {
		if _, ok := keyConstructors[name]; !ok && name != stepNameKey {
			t.Errorf("claimKeyKinds lists %s, which keyConstructors does not", name)
		}
	}

	// Each constructor's keys parse back to the table's kind.
	for name, kind := range claimKeyKinds {
		if name == stepNameKey {
			continue
		}
		for _, s := range adversarialToolUseIDs() {
			key := keyConstructors[name](s)
			got, ok := parseClaimKey(key)
			if !ok || got.kind != kind {
				t.Errorf("%s(%.30q) = %.60q parses as %+v (ok=%v), want kind %s", name, s, key, got, ok, kind)
			}
		}
	}
	for _, name := range []string{"reserve", "a:b", "node:n", "node:iter:2:H:step:N"} {
		if got, ok := parseClaimKey(name); !ok || got.kind != claimKeyKinds[stepNameKey] || got.call != name {
			t.Errorf("step name %q parses as %+v (ok=%v), want result_step", name, got, ok)
		}
	}
}

// The key parser round-trips each constructor's output to the parts it was built from.
func TestProtocolVocabulary_KeysRoundTrip(t *testing.T) {
	claim := newClaimID()
	for _, s := range adversarialToolUseIDs() {
		for _, base := range []string{toolAttemptStep(s), stepAttemptStep(s)} {
			for gen := range 4 {
				key := retryAttemptStep(base, gen)
				want := claimKey{kind: "marker", base: base}
				if gen > 0 {
					want = claimKey{kind: "retry_marker", base: base, gen: gen}
				}
				if got, ok := parseClaimKey(key); !ok || got != want {
					t.Fatalf("parse %.60q = %+v (ok=%v), want %+v", key, got, ok, want)
				}
				next, _ := parseClaimKey(nextAttemptStep(key))
				if next.kind != "retry_marker" || next.base != base || next.gen != gen+1 {
					t.Fatalf("parse nextAttemptStep(%.60q) = %+v, want attempt %d of %.60q", key, next, gen+1, base)
				}
				ns := notStartedStep(key, claim)
				wantNS := claimKey{kind: "not_started", base: base, gen: gen, claim: claim}
				if got, ok := parseClaimKey(ns); !ok || got != wantNS {
					t.Fatalf("parse %.60q = %+v (ok=%v), want %+v", ns, got, ok, wantNS)
				}
			}
		}
		key := ToolResultStep(s)
		if got, ok := parseClaimKey(key); !ok || got != (claimKey{kind: "result_tool", call: encodeID(s)}) {
			t.Fatalf("parse %.60q = %+v (ok=%v)", key, got, ok)
		}
	}
	for _, bad := range []string{"attempt:", "attempt:retry:0:tool:x", "attempt:retry:01:tool:x", "attempt:retry:x:tool:x",
		"attempt:retry:1:retry:1:tool:x", "attempt:not-started::attempt:tool:x", "attempt:not-started:c:tool:x", "attempt:other:x", "run:complete"} {
		if got, ok := parseClaimKey(bad); ok {
			t.Errorf("parse %q = %+v, want not a claim key", bad, got)
		}
	}
}
