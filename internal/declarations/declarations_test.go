package declarations

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
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
		{"a rotated approver set is a change, not a strengthening", doc(pol(twoOfTwo)), doc(pol(gate(Gate{Need: 2, Approvers: []string{"ops", "audit"}, ExcludePrincipal: true}))), []want{{Changed, "policy", "approver set changed"}}},
		{"a longer timeout is a change", doc(pol(twoOfTwo)), doc(pol(gate(Gate{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true, TimeoutMS: 60000}))), []want{{Changed, "policy", "timeout"}}},
		{"Untrusted removed", doc(pol(twoOfTwo), Tool{Name: "r", Untrusted: "crm"}), doc(pol(twoOfTwo), Tool{Name: "r"}), []want{{Weakened, "tool r", "Untrusted"}}},
		{"Untrusted added", doc(pol(twoOfTwo), Tool{Name: "r"}), doc(pol(twoOfTwo), Tool{Name: "r", Untrusted: "crm"}), []want{{Strengthened, "tool r", "Untrusted"}}},
		{"another untrusted source", doc(pol(twoOfTwo), Tool{Name: "r", Untrusted: "crm"}), doc(pol(twoOfTwo), Tool{Name: "r", Untrusted: "email"}), []want{{Changed, "tool r", "untrusted source"}}},
		{"TaintExempt added", doc(pol(twoOfTwo), Tool{Name: "w"}), doc(pol(twoOfTwo), Tool{Name: "w", Exempt: "reviewed"}), []want{{Weakened, "tool w", "TaintExempt"}}},
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
		{"a cap currency changes", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCallEUR}}), []want{{Changed, "tool w", "currency"}}},
		{"a cap line that does not parse", doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{perCall}}), doc(pol(twoOfTwo), Tool{Name: "w", Caps: []string{"nonsense"}}), []want{{Changed, "tool w", "does not parse"}}},
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
	if fs := doc(&Policy{Refuse: true}, Tool{Name: "pay", Effect: true}).Findings(); len(fs) != 0 {
		t.Errorf("a refusing policy has findings: %+v", fs)
	}
}
