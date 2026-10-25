package declarations

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/amount"
)

const (
	perCall     = `refund_one: refund.amount[minor, currency=currency] > 500 USD -> refuse`
	perCall1000 = `refund_one: refund.amount[minor, currency=currency] > 1000 USD -> refuse`
	perCallGate = `refund_one: refund.amount[minor, currency=currency] > 500 USD -> gate`
	perCallEUR  = `refund_one: refund.amount[minor, currency=currency] > 500 EUR -> refuse`
	dayTotal    = `refund_total: refund.amount[minor, currency=currency] sum 10 USD per day -> refuse`
	dayTotalRen = `refund_sum: refund.amount[minor, currency=currency] sum 10 USD per day -> refuse`
	dayTotal20  = `refund_total: refund.amount[minor, currency=currency] sum 20 USD per day -> refuse`
	hourTotal   = `refund_total: refund.amount[minor, currency=currency] sum 10 USD per hour -> refuse`
)

var twoOfTwo = &Gate{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true}

// doc builds a document with the digest of what it declares.
func doc(p *Policy, tools ...Tool) Document {
	d := Document{Format: Format, Policy: p, Tools: tools}
	d.Digest = d.Recompute()
	return d
}

func gate(g Gate) *Gate { return &g }

func TestDecode_IsStrict(t *testing.T) {
	good := doc(&Policy{Gate: twoOfTwo}, Tool{Name: "read", Untrusted: "crm"}, Tool{Name: "write", Effect: true})
	b, _ := json.Marshal(good)
	if got, err := Decode(bytes.NewReader(b)); err != nil || got.Digest != good.Digest {
		t.Fatalf("a good document: %v, %+v", err, got)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	with := func(f func(m map[string]any)) string {
		var c map[string]any
		_ = json.Unmarshal(b, &c)
		f(c)
		out, _ := json.Marshal(c)
		return string(out)
	}
	for name, in := range map[string]string{
		"an unknown member (a newer version)": with(func(m map[string]any) { m["future"] = true }),
		"an unknown member in a tool":         with(func(m map[string]any) { m["tools"].([]any)[0].(map[string]any)["future"] = 1 }),
		"another format":                      with(func(m map[string]any) { m["format"] = "bide.declarations.v2" }),
		"a digest that is not the content's":  with(func(m map[string]any) { m["digest"] = strings.Repeat("0", 64) }),
		"tools out of order":                  with(func(m map[string]any) { ts := m["tools"].([]any); ts[0], ts[1] = ts[1], ts[0] }),
		"a tool twice":                        with(func(m map[string]any) { ts := m["tools"].([]any); m["tools"] = []any{ts[0], ts[0]} }),
		"two documents":                       string(b) + string(b),
		"not JSON":                            "policy",
	} {
		if _, err := Decode(strings.NewReader(in)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A document with no policy has no digest, and every tool is still listed.
func TestRecompute_NoPolicyNoDigest(t *testing.T) {
	if d := doc(nil, Tool{Name: "write", Effect: true, Untrusted: "x"}); d.Digest != "" {
		t.Fatalf("digest = %q, want none without a policy", d.Digest)
	}
}

func TestRecompute_CoversExactlyWhatTheDigestCovers(t *testing.T) {
	base := doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, Caps: []string{perCall}})
	same := []Document{
		doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: false, Caps: []string{perCall}}),                                   // Effect is not covered
		doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, OwnGate: gate(Gate{Single: true}), Caps: []string{perCall}}), // nor the tool's own gate
		doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, Caps: []string{perCall}}, Tool{Name: "zz-plain"}),            // nor a tool with nothing declared
	}
	for i, d := range same {
		if d.Digest != base.Digest {
			t.Errorf("variant %d changed the digest, but it changes nothing the digest covers", i)
		}
	}
	differ := []Document{
		doc(&Policy{Gate: twoOfTwo, Refuse: true}, Tool{Name: "write", Caps: []string{perCall}}),
		doc(&Policy{Gate: gate(Gate{Need: 2, Approvers: []string{"ops", "risk"}})}, Tool{Name: "write", Caps: []string{perCall}}),
		doc(&Policy{Gate: gate(Gate{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true, TimeoutMS: 5})}, Tool{Name: "write", Caps: []string{perCall}}),
		doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Caps: []string{perCall1000}}),
		doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Untrusted: "x", Caps: []string{perCall}}),
		doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Exempt: "r", Caps: []string{perCall}}),
	}
	for i, d := range differ {
		if d.Digest == base.Digest {
			t.Errorf("variant %d left the digest unchanged", i)
		}
	}
}

type want struct {
	class   Class
	subject string
	has     string // a substring of the message
}

func TestDiff_Classifies(t *testing.T) {
	pol := func(g *Gate) *Policy { return &Policy{Gate: g} }
	for _, tc := range []struct {
		name     string
		old, new Document
		want     []want
	}{
		{"nothing changed", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), nil},
		{"the policy is removed", doc(pol(twoOfTwo)), doc(nil), []want{{Weakened, "policy", "removed"}}},
		{"a policy is added", doc(nil), doc(pol(twoOfTwo)), []want{{Strengthened, "policy", "added"}}},
		{"refuse becomes a gate", doc(&Policy{Refuse: true}), doc(pol(twoOfTwo)), []want{{Weakened, "policy", "escalation gate"}}},
		{"a gate becomes refuse", doc(pol(twoOfTwo)), doc(&Policy{Refuse: true}), []want{{Strengthened, "policy", "refuse"}}},
		{"need lowered", doc(pol(twoOfTwo)), doc(pol(gate(Gate{Need: 1, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true}))), []want{{Weakened, "policy", "need lowered"}}},
		{"need raised", doc(pol(gate(Gate{Need: 1, Approvers: []string{"a", "b", "c"}}))), doc(pol(gate(Gate{Need: 2, Approvers: []string{"a", "b", "c"}}))), []want{{Strengthened, "policy", "need raised"}}},
		{"the requester is no longer excluded", doc(pol(twoOfTwo)), doc(pol(gate(Gate{Need: 2, Approvers: []string{"ops", "risk"}}))), []want{{Weakened, "policy", "no longer excluded"}}},
		{"m-of-n becomes one decision", doc(pol(twoOfTwo)), doc(pol(gate(Gate{Single: true}))), []want{{Weakened, "policy", "one decision"}}},
		{"one decision becomes m-of-n", doc(pol(gate(Gate{Single: true}))), doc(pol(twoOfTwo)), []want{{Strengthened, "policy", "m-of-n"}}},
		{"RequireProvenance is dropped", doc(&Policy{Refuse: true, RequireProvenance: true}), doc(&Policy{Refuse: true}), []want{{Weakened, "policy", "no longer required"}}},
		{"RequireProvenance is turned on", doc(&Policy{Refuse: true}), doc(&Policy{Refuse: true, RequireProvenance: true}), []want{{Strengthened, "policy", "must now declare"}}},
		{"a trusted output is declared", doc(pol(twoOfTwo), Tool{Name: "t"}), doc(pol(twoOfTwo), Tool{Name: "t", Trusted: "ours"}), []want{{Changed, "tool t", "trusted output is declared"}}},
		{"a trusted output is removed", doc(pol(twoOfTwo), Tool{Name: "t", Trusted: "ours"}), doc(pol(twoOfTwo), Tool{Name: "t"}), []want{{Changed, "tool t", "declaration is removed"}}},
		{"a trusted output replaces Untrusted", doc(pol(twoOfTwo), Tool{Name: "t", Untrusted: "crm"}), doc(pol(twoOfTwo), Tool{Name: "t", Trusted: "ours"}), []want{{Weakened, "tool t", "Untrusted"}, {Changed, "tool t", "trusted output is declared"}}},
		{"a rotated approver set is weakened: a person must approve it", doc(pol(twoOfTwo)), doc(pol(gate(Gate{Need: 2, Approvers: []string{"ops", "audit"}, ExcludePrincipal: true}))), []want{{Weakened, "policy", "removed or replaced"}}},
		{"an approver removed while need is still met", doc(pol(gate(Gate{Need: 2, Approvers: []string{"a", "b", "c"}, ExcludePrincipal: true}))), doc(pol(gate(Gate{Need: 2, Approvers: []string{"a", "b"}, ExcludePrincipal: true}))), []want{{Weakened, "policy", "removed or replaced"}}},
		{"the same approvers in another order", doc(pol(gate(Gate{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true}))), doc(pol(gate(Gate{Need: 2, Approvers: []string{"risk", "ops"}, ExcludePrincipal: true}))), []want{{Changed, "policy", "approver order changed"}}},
		{"an approver only added", doc(pol(twoOfTwo)), doc(pol(gate(Gate{Need: 2, Approvers: []string{"ops", "risk", "audit"}, ExcludePrincipal: true}))), []want{{Strengthened, "policy", "approvers were added"}}},
		{"a longer timeout is a change", doc(pol(twoOfTwo)), doc(pol(gate(Gate{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true, TimeoutMS: 60000}))), []want{{Changed, "policy", "timeout"}}},
		{"Untrusted removed", doc(pol(twoOfTwo), Tool{Name: "r", Untrusted: "crm"}), doc(pol(twoOfTwo), Tool{Name: "r"}), []want{{Weakened, "tool r", "Untrusted"}}},
		{"Untrusted added", doc(pol(twoOfTwo), Tool{Name: "r"}), doc(pol(twoOfTwo), Tool{Name: "r", Untrusted: "crm"}), []want{{Strengthened, "tool r", "Untrusted"}}},
		{"another untrusted source", doc(pol(twoOfTwo), Tool{Name: "r", Untrusted: "crm"}), doc(pol(twoOfTwo), Tool{Name: "r", Untrusted: "email"}), []want{{Changed, "tool r", "untrusted source"}}},
		{"TaintExempt added", doc(pol(twoOfTwo), Tool{Name: "w"}), doc(pol(twoOfTwo), Tool{Name: "w", Exempt: "reviewed"}), []want{{Weakened, "tool w", "TaintExempt(\"reviewed\") added"}}},
		{"TaintExempt removed", doc(pol(twoOfTwo), Tool{Name: "w", Exempt: "reviewed"}), doc(pol(twoOfTwo), Tool{Name: "w"}), []want{{Strengthened, "tool w", "TaintExempt removed"}}},
		{"TaintExempt reworded", doc(pol(twoOfTwo), Tool{Name: "w", Exempt: "reviewed"}), doc(pol(twoOfTwo), Tool{Name: "w", Exempt: "approved by risk"}), []want{{Weakened, "tool w", "reason changed"}}},
		{"a new exempt tool", doc(pol(twoOfTwo)), doc(pol(twoOfTwo), Tool{Name: "w", Exempt: "x"}), []want{{Weakened, "tool w", "added with TaintExempt"}}},
		{"a tool with Untrusted is removed", doc(pol(twoOfTwo), Tool{Name: "r", Untrusted: "crm"}), doc(pol(twoOfTwo)), []want{{Weakened, "tool r", "removed"}}},
		{"a tool with caps is removed", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), doc(pol(twoOfTwo)), []want{{Weakened, "tool w", "cap rules"}}},
		{"a cap limit is raised", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall1000}}), []want{{Weakened, "tool w", "limit raised"}}},
		{"a cap limit is lowered", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall1000}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), []want{{Strengthened, "tool w", "limit lowered"}}},
		{"a cap rule is removed", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall, dayTotal}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), []want{{Weakened, "tool w", `"refund_total" removed`}}},
		{"a cap rule is added", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall, dayTotal}}), []want{{Strengthened, "tool w", `"refund_total" added`}}},
		{"a cumulative rule is renamed", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{dayTotal}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{dayTotalRen}}), []want{{Weakened, "tool w", "starts again"}}},
		{"a cap action goes from refuse to gate", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCallGate}}), []want{{Weakened, "tool w", "refuse becomes a gate"}}},
		{"a cap window goes from an hour to a day", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{hourTotal}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{dayTotal}}), []want{{Weakened, "tool w", "a day, was an hour"}}},
		{"a cumulative limit is raised", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{dayTotal}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{dayTotal20}}), []want{{Weakened, "tool w", "limit raised"}}},
		{"a cap currency changes", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCallEUR}}), []want{{Weakened, "tool w", "currency"}}},
		{"a cap line that does not parse", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{"nonsense"}}), []want{{Weakened, "tool w", "do not parse"}}},
		{"the old cap lines do not parse", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{"nonsense"}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), []want{{Weakened, "tool w", "old cap lines do not parse"}}},
		{"a cumulative key is replaced", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{`refund_total: refund.amount[minor, currency=currency] sum 10 USD per day by customer -> refuse`}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{`refund_total: refund.amount[minor, currency=currency] sum 10 USD per day by region -> refuse`}}), []want{{Weakened, "tool w", "key changed"}}},
		{"a currency swap hides a raised limit", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{`refund_one: refund.amount[minor, currency=currency] > 5000000 EUR -> refuse`}}), []want{{Weakened, "tool w", "cannot be compared"}}},
		{"a per-call cap replaced by a huge cumulative one", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{`refund_total: refund.amount[minor, currency=currency] > 500 USD -> refuse`}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{`refund_total: refund.amount[minor, currency=currency] sum 99999999 USD per day -> refuse`}}), []want{{Weakened, "tool w", "kind changed"}}},
		{"a key is added to a cumulative rule", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{dayTotal}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{`refund_total: refund.amount[minor, currency=currency] sum 10 USD per day by customer -> refuse`}}), []want{{Weakened, "tool w", "key is added"}}},
		{"a key is removed from a cumulative rule", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{`refund_total: refund.amount[minor, currency=currency] sum 10 USD per day by customer -> refuse`}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{dayTotal}}), []want{{Strengthened, "tool w", "key is removed"}}},
		{"a tool becomes ReadOnly", doc(pol(twoOfTwo), Tool{Name: "w", Effect: true}), doc(pol(twoOfTwo), Tool{Name: "w"}), []want{{Weakened, "tool w", "no longer has side effects"}}},
		{"a tool gains side effects", doc(pol(twoOfTwo), Tool{Name: "w"}), doc(pol(twoOfTwo), Tool{Name: "w", Effect: true}), []want{{Strengthened, "tool w", "now has side effects"}}},
		{"RaisesSafety is added", doc(pol(twoOfTwo), Tool{Name: "w", Effect: true}), doc(pol(twoOfTwo), Tool{Name: "w", Effect: true, RaisesSafety: "dry run"}), []want{{Weakened, "tool w", `RaisesSafety("dry run") added`}}},
		{"RaisesSafety is removed", doc(pol(twoOfTwo), Tool{Name: "w", Effect: true, RaisesSafety: "dry run"}), doc(pol(twoOfTwo), Tool{Name: "w", Effect: true}), []want{{Strengthened, "tool w", "RaisesSafety removed"}}},
		{"RaisesSafety is reworded", doc(pol(twoOfTwo), Tool{Name: "w", Effect: true, RaisesSafety: "dry run"}), doc(pol(twoOfTwo), Tool{Name: "w", Effect: true, RaisesSafety: "replica"}), []want{{Changed, "tool w", `reason is "replica", was "dry run"`}}},
		{"a wrapper raised to ReadOnly under RaisesSafety", doc(pol(twoOfTwo), Tool{Name: "w", Effect: true}), doc(pol(twoOfTwo), Tool{Name: "w", RaisesSafety: "dry run"}), []want{{Weakened, "tool w", "RaisesSafety"}, {Weakened, "tool w", "no longer has side effects"}}},
		{"a wrapper raised to Idempotent under RaisesSafety", doc(pol(twoOfTwo), Tool{Name: "w", Effect: true}), doc(pol(twoOfTwo), Tool{Name: "w", Effect: true, Idempotent: true, RaisesSafety: "keyed"}), []want{{Weakened, "tool w", "RaisesSafety"}, {Weakened, "tool w", "now Idempotent"}}},
		{"a tool becomes Idempotent without it", doc(pol(twoOfTwo), Tool{Name: "w", Effect: true}), doc(pol(twoOfTwo), Tool{Name: "w", Effect: true, Idempotent: true}), []want{{Weakened, "tool w", "now Idempotent"}}},
		{"a tool stops being Idempotent", doc(pol(twoOfTwo), Tool{Name: "w", Effect: true, Idempotent: true}), doc(pol(twoOfTwo), Tool{Name: "w", Effect: true}), []want{{Strengthened, "tool w", "no longer Idempotent"}}},
		{"Idempotent becomes ReadOnly", doc(pol(twoOfTwo), Tool{Name: "w", Effect: true, Idempotent: true}), doc(pol(twoOfTwo), Tool{Name: "w"}), []want{{Weakened, "tool w", "no longer has side effects"}}},
		{"ReadOnly becomes Idempotent", doc(pol(twoOfTwo), Tool{Name: "w"}), doc(pol(twoOfTwo), Tool{Name: "w", Effect: true, Idempotent: true}), []want{{Strengthened, "tool w", "now has side effects"}}},
		{"a new tool with RaisesSafety", doc(pol(twoOfTwo)), doc(pol(twoOfTwo), Tool{Name: "w", RaisesSafety: "dry run"}), []want{{Weakened, "tool w", "added with RaisesSafety"}}},
		{"a tool with RaisesSafety is removed", doc(pol(twoOfTwo), Tool{Name: "w", RaisesSafety: "dry run"}), doc(pol(twoOfTwo)), []want{{Strengthened, "tool w", "it was RaisesSafety"}}},
		{"an approver set replaced outright", doc(pol(twoOfTwo)), doc(pol(gate(Gate{Need: 2, Approvers: []string{"mallory", "x"}, ExcludePrincipal: true}))), []want{{Weakened, "policy", "none in common"}}},
		{"a tool's own gate is weakened (not covered by the digest)", doc(pol(twoOfTwo), Tool{Name: "w", OwnGate: twoOfTwo}), doc(pol(twoOfTwo), Tool{Name: "w", OwnGate: gate(Gate{Single: true})}), []want{{Weakened, "tool w own gate", "one decision"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Diff(tc.old, tc.new)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("changes = %+v, want %d", got, len(tc.want))
			}
			for i, w := range tc.want {
				if got[i].Class != w.class || got[i].Subject != w.subject || !strings.Contains(got[i].What, w.has) {
					t.Errorf("change %d = %+v, want %v %q containing %q", i, got[i], w.class, w.subject, w.has)
				}
			}
		})
	}
}

// The digest and the declarations must agree about what changed, in both directions.
func TestDiff_RefusesADumpTheDigestDisagreesWith(t *testing.T) {
	a := doc(&Policy{Gate: twoOfTwo}, Tool{Name: "w", Caps: []string{perCall}})
	b := doc(&Policy{Gate: twoOfTwo}, Tool{Name: "w", Caps: []string{perCall1000}})

	// Different digests with no difference: the digest covers something the declarations do not carry.
	lying := a
	lying.Digest = b.Digest
	if _, err := Diff(a, lying); err == nil {
		t.Error("a document whose digest is not the digest of its content was diffed")
	}
	// Equal digests with a covered difference.
	forged := b
	forged.Digest = a.Digest
	if _, err := Diff(a, forged); err == nil {
		t.Error("equal digests with different covered declarations were diffed")
	}
	// The two checks themselves, with documents that each pass Recompute: build the cases directly.
	same := doc(&Policy{Gate: twoOfTwo})
	onlyEffect := doc(&Policy{Gate: twoOfTwo}, Tool{Name: "x", Effect: true})
	if got, err := Diff(same, onlyEffect); err != nil || len(got) != 1 || got[0].Covered {
		t.Errorf("a difference the digest does not cover: %v %+v (want one uncovered change and no error)", err, got)
	}
	// Equal digests and no difference is fine; so is no policy on both sides.
	if got, err := Diff(a, a); err != nil || len(got) != 0 {
		t.Errorf("identical documents: %v %+v", err, got)
	}
	n1, n2 := doc(nil, Tool{Name: "r", Untrusted: "a"}), doc(nil, Tool{Name: "r"})
	if got, err := Diff(n1, n2); err != nil || len(got) != 1 {
		t.Errorf("no policy on either side: %v %+v", err, got)
	}
}

// What the digest covers and what the diff reports are one list: for every variant of a document, Diff
// shows a covered change exactly when the digests differ. The two checks inside Diff turn a gap between
// them (a declaration the digest covers and the diff skips, or the reverse) into an error.
func TestDiff_CoveredChangesAreExactlyTheDigestsDifferences(t *testing.T) {
	base := doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"})
	variants := map[string]Document{
		"refuse":        doc(&Policy{Gate: twoOfTwo, Refuse: true}, Tool{Name: "write", Effect: true, Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"}),
		"timeout":       doc(&Policy{Gate: gate(Gate{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true, TimeoutMS: 9})}, Tool{Name: "write", Effect: true, Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"}),
		"caps":          doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, Caps: []string{perCall, dayTotal}}, Tool{Name: "read", Untrusted: "crm"}),
		"untrusted":     doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "email"}),
		"exempt":        doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, Exempt: "x", Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"}),
		"tool added":    doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"}, Tool{Name: "zz", Exempt: "y"}),
		"effect":        doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"}),
		"own gate":      doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, OwnGate: twoOfTwo, Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"}),
		"plain tool":    doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"}, Tool{Name: "zz"}),
		"raises safety": doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, RaisesSafety: "x", Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"}),
		"idempotent":    doc(&Policy{Gate: twoOfTwo}, Tool{Name: "write", Effect: true, Idempotent: true, Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"}),
		"approver swap": doc(&Policy{Gate: gate(Gate{Need: 2, Approvers: []string{"ops", "x"}, ExcludePrincipal: true})}, Tool{Name: "write", Effect: true, Caps: []string{perCall}}, Tool{Name: "read", Untrusted: "crm"}),
	}
	for name, v := range variants {
		changes, err := Diff(base, v)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		covered := false
		for _, c := range changes {
			covered = covered || c.Covered
		}
		if covered != (base.Digest != v.Digest) {
			t.Errorf("%s: covered changes %v, digests differ %v", name, covered, base.Digest != v.Digest)
		}
		if len(changes) == 0 {
			t.Errorf("%s: a variant with a difference shows none", name)
		}
	}
}

func TestFindings(t *testing.T) {
	has := func(fs []Finding, subject, part string) bool {
		for _, f := range fs {
			if f.Subject == subject && strings.Contains(f.Message, part) {
				return true
			}
		}
		return false
	}
	noPolicy := doc(nil, Tool{Name: "pay", Effect: true}, Tool{Name: "read", Untrusted: "crm"}, Tool{Name: "look"})
	fs := noPolicy.Findings()
	if !has(fs, "tool pay", "no taint policy") || !has(fs, "tool read", "marks are recorded") || has(fs, "tool look", "") {
		t.Errorf("no policy: %+v", fs)
	}
	if fs := doc(&Policy{Gate: gate(Gate{Single: true})}).Findings(); !has(fs, "policy", "one decision") {
		t.Errorf("a single gate: %+v", fs)
	}
	if fs := doc(&Policy{Gate: gate(Gate{Need: 1, Approvers: []string{"a", "b"}, ExcludePrincipal: true})}).Findings(); !has(fs, "policy", "need >= 2") {
		t.Errorf("need 1: %+v", fs)
	}
	if fs := doc(&Policy{Gate: gate(Gate{Need: 2, Approvers: []string{"a", "b"}})}).Findings(); !has(fs, "policy", "does not exclude the requester") {
		t.Errorf("no ExcludePrincipal: %+v", fs)
	}
	if fs := doc(&Policy{Gate: twoOfTwo}, Tool{Name: "w", Exempt: "reviewed", Caps: []string{perCall}}).Findings(); !has(fs, "tool w", "exempt") || !has(fs, "tool w", "cap rule") || has(fs, "policy", "") {
		t.Errorf("a bank policy with an exempt capped tool: %+v", fs)
	}
	// A tool whose provenance nobody stated is reported while the policy does not require one; a classified
	// tool, and a policy that requires provenance, are not.
	un := doc(&Policy{Refuse: true}, Tool{Name: "pay", Effect: true}, Tool{Name: "crm", Untrusted: "crm"}, Tool{Name: "ours", Trusted: "our table"})
	if fs := un.Findings(); !has(fs, "tool pay", "declares no provenance") || has(fs, "tool crm", "") || has(fs, "tool ours", "") {
		t.Errorf("unclassified tool findings: %+v", fs)
	}
	if fs := doc(&Policy{Refuse: true, RequireProvenance: true}, Tool{Name: "pay", Effect: true}).Findings(); len(fs) != 0 {
		t.Errorf("a policy that requires provenance has findings: %+v", fs)
	}
	if fs := doc(nil, Tool{Name: "plain"}).Findings(); has(fs, "tool plain", "declares no provenance") {
		t.Errorf("a document with no policy reports the provenance of a tool: %+v", fs)
	}
}

// The two checks that hold the digests to the covered changes, each way (Diff's own output cannot reach
// them while the diff and the digest agree, so they are tested on their own).
func TestCheckDigests(t *testing.T) {
	covered := []Change{{Class: Weakened, Covered: true}}
	uncovered := []Change{{Class: Changed}}
	for _, tc := range []struct {
		name         string
		a, b         string
		changes      []Change
		wantErr      bool
		wantContains string
	}{
		{"different digests, nothing covered differs", "aaa", "bbb", nil, true, "covers something the declarations do not carry"},
		{"different digests, only uncovered differences", "aaa", "bbb", uncovered, true, "covers something the declarations do not carry"},
		{"equal digests, a covered difference", "aaa", "aaa", covered, true, "does not cover"},
		{"different digests, a covered difference", "aaa", "bbb", covered, false, ""},
		{"equal digests, only an uncovered difference", "aaa", "aaa", uncovered, false, ""},
		{"equal digests, no difference", "aaa", "aaa", nil, false, ""},
		{"no policy on either side", "", "", covered, false, ""},
		{"a policy on one side", "", "bbb", covered, false, ""},
	} {
		err := checkDigests(tc.a, tc.b, tc.changes)
		if (err != nil) != tc.wantErr || err != nil && !strings.Contains(err.Error(), tc.wantContains) {
			t.Errorf("%s: err = %v, want error %v containing %q", tc.name, err, tc.wantErr, tc.wantContains)
		}
	}
}

// Encode and Decode are inverse, and the encoding keeps a cap line readable.
func TestEncode_RoundTripsAndKeepsCapLinesReadable(t *testing.T) {
	d := doc(&Policy{Gate: twoOfTwo}, Tool{Name: "w", Effect: true, Caps: []string{perCall}})
	var buf bytes.Buffer
	if err := Encode(&buf, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "> 500 USD -> refuse") || strings.Contains(buf.String(), `\u003e`) {
		t.Fatalf("the cap line is escaped: %s", buf.String())
	}
	back, err := Decode(&buf)
	if err != nil || back.Digest != d.Digest || len(back.Tools) != 1 {
		t.Fatalf("round trip: %v %+v", err, back)
	}
	var empty bytes.Buffer
	if err := Encode(&empty, doc(nil)); err != nil || !strings.Contains(empty.String(), `"tools": []`) {
		t.Fatalf("a document with no tool encodes %q (%v), want an empty list", empty.String(), err)
	}
}

// The document the agent dumps (the golden is written by the agent's test) reads back strictly.
func TestDecode_ReadsTheAgentsGolden(t *testing.T) {
	f, err := os.Open("testdata/agent.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d, err := Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if d.Policy == nil || len(d.Tools) != 2 || d.Digest == "" {
		t.Fatalf("golden = %+v", d)
	}
}

// Two rules whose arguments print the same with a space are different rules.
func TestRuleKey_DoesNotConflateArgumentsThatPrintAlike(t *testing.T) {
	a := amount.Rule{Name: "r", Tools: []string{"refund"}, Arg: []string{"a b"}}
	b := amount.Rule{Name: "r", Tools: []string{"refund"}, Arg: []string{"a", "b"}}
	c := amount.Rule{Name: "r", Tools: []string{"refund x"}}
	d := amount.Rule{Name: "r", Tools: []string{"refund", "x"}}
	if ruleKey(a) == ruleKey(b) || ruleKey(c) == ruleKey(d) {
		t.Fatal("rules that differ in how their members split print the same")
	}
}

// What the reader sees in the text is what the document holds: a duplicate member, a member in another case,
// and a document over the size limit are refused.
func TestDecode_RefusesWhatEncodingJSONWouldReadAsSomethingElse(t *testing.T) {
	good := doc(&Policy{Gate: twoOfTwo}, Tool{Name: "w", Caps: []string{perCall}})
	var buf bytes.Buffer
	if err := Encode(&buf, good); err != nil {
		t.Fatal(err)
	}
	base := buf.String()
	for name, in := range map[string]string{
		"a duplicate tools member":        strings.Replace(base, `"tools": [`, `"tools": [], "tools": [`, 1),
		"FORMAT for format":               strings.Replace(base, `"format"`, `"FORMAT"`, 1),
		"Tools beside tools":              strings.Replace(base, `"tools": [`, `"Tools": [], "tools": [`, 1),
		"a duplicate caps array":          strings.Replace(base, `"caps": [`, `"caps": [], "caps": [`, 1),
		"a duplicate name in a tool":      strings.Replace(base, `"name": "w"`, `"name": "w", "name": "w"`, 1),
		"a duplicate digest":              strings.Replace(base, `"digest": `, `"digest": "", "digest": `, 1),
		"invalid UTF-8":                   strings.Replace(base, `"name": "w"`, "\"name\": \"w\xff\"", 1),
		"a null where a tool list stands": strings.Replace(base, `"tools": [`, `"tools": null, "x": [`, 1),
	} {
		if _, err := Decode(strings.NewReader(in)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	big := strings.Repeat(" ", 16<<20+1)
	if _, err := Decode(strings.NewReader(big)); err == nil || !strings.Contains(err.Error(), "over") {
		t.Errorf("a document over the limit: %v, want a size error", err)
	}
	if _, err := Decode(strings.NewReader(base)); err != nil {
		t.Fatalf("the control document is refused: %v", err)
	}
}

// The golden is compared byte for byte with what the agent writes (LF), so a Windows checkout must not
// turn its line endings into CRLF: .gitattributes pins them. A missing rule fails here, on any platform,
// rather than only on a machine whose checkout converts.
func TestGolden_IsPinnedToLF(t *testing.T) {
	raw, err := os.ReadFile("../../.gitattributes")
	if err != nil {
		t.Fatal(err)
	}
	pinned := false
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && !strings.HasPrefix(f[0], "#") && f[0] == "internal/declarations/testdata/*.json" {
			for _, attr := range f[1:] {
				pinned = pinned || attr == "eol=lf" || attr == "-text" || attr == "binary"
			}
		}
	}
	if !pinned {
		t.Fatal("internal/declarations/testdata/*.json is not pinned to LF in .gitattributes: a Windows checkout would break the agent's golden comparison")
	}
	if b, err := os.ReadFile("testdata/agent.golden.json"); err != nil || bytes.Contains(b, []byte("\r")) {
		t.Fatalf("the golden holds a carriage return (%v)", err)
	}
}

// A document holds only what a valid agent can produce: the gate checks agent.ApprovalPolicy.Validate makes.
// A duplicate approver would count twice toward need, so a document that lists one is refused, not reported.
func TestDecode_RefusesAGateNoValidAgentHas(t *testing.T) {
	for name, g := range map[string]*Gate{
		"a duplicate approver":                      {Need: 2, Approvers: []string{"a", "a", "b"}},
		"approvers that differ only by case":        {Need: 2, Approvers: []string{"Alice", "alice"}},
		"need above the approvers":                  {Need: 3, Approvers: []string{"a", "b"}},
		"need zero":                                 {Need: 0, Approvers: []string{"a"}},
		"no approvers":                              {Need: 1},
		"an empty approver":                         {Need: 1, Approvers: []string{""}},
		"an approver with a space":                  {Need: 1, Approvers: []string{"a b"}},
		"a negative timeout":                        {Need: 1, Approvers: []string{"a"}, TimeoutMS: -1},
		"a one-decision gate with approvers":        {Single: true, Approvers: []string{"a"}},
		"a one-decision gate excluding a requester": {Single: true, ExcludePrincipal: true},
	} {
		for where, d := range map[string]Document{
			"the policy":   doc(&Policy{Gate: g}),
			"a tool's own": doc(&Policy{Gate: twoOfTwo}, Tool{Name: "w", OwnGate: g}),
		} {
			var buf bytes.Buffer
			if err := Encode(&buf, d); err != nil {
				t.Fatal(err)
			}
			if _, err := Decode(&buf); err == nil {
				t.Errorf("%s in %s was accepted", name, where)
			}
		}
	}
	var buf bytes.Buffer
	_ = Encode(&buf, doc(&Policy{}))
	if _, err := Decode(&buf); err == nil {
		t.Error("a policy with neither a gate nor refuse was accepted")
	}
}

// A tool that is both untrusted and trusted is not a document a valid agent writes.
func TestDecode_RefusesAToolThatIsBothUntrustedAndTrusted(t *testing.T) {
	d := doc(&Policy{Refuse: true}, Tool{Name: "t", Untrusted: "crm", Trusted: "ours"})
	var buf bytes.Buffer
	if err := Encode(&buf, d); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(&buf); err == nil {
		t.Error("a tool that is both untrusted and trusted was accepted")
	}
}

// The digest covers the requirement and the trusted declaration, and only when they are set.
func TestRecompute_CoversProvenance(t *testing.T) {
	base := doc(&Policy{Refuse: true}, Tool{Name: "t"})
	for name, d := range map[string]Document{
		"the requirement": doc(&Policy{Refuse: true, RequireProvenance: true}, Tool{Name: "t"}),
		"a trusted tool":  doc(&Policy{Refuse: true}, Tool{Name: "t", Trusted: "ours"}),
	} {
		if d.Digest == base.Digest {
			t.Errorf("%s is not in the digest", name)
		}
	}
	if doc(&Policy{Refuse: true}, Tool{Name: "t"}).Digest != base.Digest {
		t.Error("the same declarations give another digest")
	}
}

// A trusted reason that says nothing (blank, non-printing, oversized) is not a document a valid agent writes.
func TestDecode_RefusesAnEmptyTrustedReason(t *testing.T) {
	for _, r := range []string{" ", "\t", "​", " ", strings.Repeat("x", 129)} {
		var buf bytes.Buffer
		if err := Encode(&buf, doc(&Policy{Refuse: true}, Tool{Name: "t", Trusted: r})); err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(&buf); err == nil {
			t.Errorf("a trusted reason %q was accepted", r)
		}
	}
	var buf bytes.Buffer
	_ = Encode(&buf, doc(&Policy{Refuse: true}, Tool{Name: "t", Trusted: "the host's table"}))
	if _, err := Decode(&buf); err != nil {
		t.Errorf("a real trusted reason: %v", err)
	}
}

// The digest covers RaisesSafety (only when set); Idempotent is for the diff and changes no digest.
func TestRecompute_CoversRaisesSafety(t *testing.T) {
	base := doc(&Policy{Refuse: true}, Tool{Name: "t", Effect: true})
	raised := doc(&Policy{Refuse: true}, Tool{Name: "t", Effect: true, RaisesSafety: "dry run"})
	other := doc(&Policy{Refuse: true}, Tool{Name: "t", Effect: true, RaisesSafety: "replica"})
	if base.Digest == raised.Digest || raised.Digest == other.Digest {
		t.Error("RaisesSafety, or its reason, is not in the digest")
	}
	if idem := doc(&Policy{Refuse: true}, Tool{Name: "t", Effect: true, Idempotent: true}); idem.Digest != base.Digest {
		t.Error("Idempotent is in the digest")
	}
}

// A RaisesSafety reason follows the rules of agent.Trusted, and Idempotent needs a side effect.
func TestDecode_RefusesABadRaisesSafetyReasonOrAnIdempotentReadOnlyTool(t *testing.T) {
	for _, r := range []string{" ", "\t", "\u200b", strings.Repeat("x", 129)} {
		var buf bytes.Buffer
		if err := Encode(&buf, doc(&Policy{Refuse: true}, Tool{Name: "t", RaisesSafety: r})); err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(&buf); err == nil {
			t.Errorf("a RaisesSafety reason %q was accepted", r)
		}
	}
	var buf bytes.Buffer
	_ = Encode(&buf, doc(&Policy{Refuse: true}, Tool{Name: "t", Idempotent: true}))
	if _, err := Decode(&buf); err == nil {
		t.Error("an Idempotent tool with no side effect was accepted")
	}
	buf.Reset()
	_ = Encode(&buf, doc(&Policy{Refuse: true}, Tool{Name: "t", Effect: true, Idempotent: true, RaisesSafety: "it stops at a dry run"}))
	if _, err := Decode(&buf); err != nil {
		t.Errorf("a valid RaisesSafety tool: %v", err)
	}
}

// bide doctor -declarations lists a RaisesSafety tool.
func TestFindings_ListsRaisesSafety(t *testing.T) {
	for _, f := range doc(&Policy{Refuse: true, RequireProvenance: true}, Tool{Name: "w", Trusted: "ours", RaisesSafety: "dry run"}).Findings() {
		if f.Subject == "tool w" && strings.Contains(f.Message, "RaisesSafety") && strings.Contains(f.Message, "dry run") {
			return
		}
	}
	t.Error("no finding for a tool that declares RaisesSafety")
}

// A dump that changes only a member outside the digest (Idempotent) has equal digests and a non-empty diff;
// that is not the digest-consistency error, because the change is reported as not covered.
func TestDiff_AMemberOutsideTheDigestIsNotADigestMismatch(t *testing.T) {
	pol := &Policy{Refuse: true}
	old := doc(pol, Tool{Name: "w", Effect: true})
	nw := doc(pol, Tool{Name: "w", Effect: true, Idempotent: true})
	if old.Digest != nw.Digest {
		t.Fatal("Idempotent changed the digest")
	}
	changes, err := Diff(old, nw)
	if err != nil {
		t.Fatalf("Diff = %v, want no digest-consistency error", err)
	}
	if len(changes) != 1 || changes[0].Covered || changes[0].Class != Weakened {
		t.Errorf("changes = %+v, want one uncovered change", changes)
	}
}

// tracked marks a document as written by a version that carries each tool's Idempotent member.
func tracked(d Document) Document { d.TracksIdempotent = true; return d }

// A baseline from before the idempotent member says nothing about it: its absence is not "false". Diffing it
// against a new dump with an Idempotent tool gives one line saying to regenerate it (weakened, so a pipeline
// stops), never a per-tool "now Idempotent". With the member on both sides the tool rows come back.
func TestDiff_AnOldDumpWithoutTheIdempotentMemberIsNotReadAsFalse(t *testing.T) {
	pol := &Policy{Refuse: true}
	oldDump := doc(pol, Tool{Name: "w", Effect: true})
	newDump := tracked(doc(pol, Tool{Name: "w", Effect: true, Idempotent: true}))
	got, err := Diff(oldDump, newDump)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Class != Weakened || got[0].Subject != "declarations" || !strings.Contains(got[0].What, "regenerate the baseline") || got[0].Covered {
		t.Fatalf("changes = %+v, want one weakened line saying to regenerate the baseline", got)
	}
	// The other direction (a new dump from an older version) is the same.
	if got, err := Diff(newDump, oldDump); err != nil || len(got) != 1 || got[0].Subject != "declarations" {
		t.Errorf("reversed: %+v, %v", got, err)
	}
	// Both with the member: the tool row, and no regenerate line.
	got, err = Diff(tracked(doc(pol, Tool{Name: "w", Effect: true})), newDump)
	if err != nil || len(got) != 1 || got[0].Subject != "tool w" || !strings.Contains(got[0].What, "now Idempotent") {
		t.Errorf("both tracked: %+v, %v", got, err)
	}
	// No Idempotent tool anywhere: an old dump costs nothing.
	if got, err := Diff(oldDump, tracked(doc(pol, Tool{Name: "w", Effect: true}))); err != nil || len(got) != 0 {
		t.Errorf("no Idempotent tool: %+v, %v", got, err)
	}
}
