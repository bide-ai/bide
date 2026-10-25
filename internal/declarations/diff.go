package declarations

import (
	"fmt"
	"maps"
	"slices"

	"github.com/bide-ai/bide/internal/amount"
)

// Class says which way a change moves what the declarations require.
type Class int

const (
	// Changed is a difference that is neither plainly looser nor plainly stricter (a gate timeout, another
	// untrusted source name, an amount member), and that a person should read. Anything the report cannot
	// judge as safe is Weakened, not Changed: a pipeline gate must stop on it.
	Changed Class = iota
	// Strengthened requires more than before.
	Strengthened
	// Weakened requires less than before: a raised threshold, a removed or renamed rule, a removed
	// Untrusted, a new TaintExempt, a weaker gate.
	Weakened
)

// String is the class as the report prints it: weakened, strengthened or changed.
func (c Class) String() string {
	switch c {
	case Strengthened:
		return "strengthened"
	case Weakened:
		return "weakened"
	}
	return "changed"
}

// Change is one difference between two documents.
type Change struct {
	Class   Class
	Subject string // "policy", or "tool <name>"
	What    string
	// Covered says the digest covers the declaration (the policy, and a tool's Untrusted, Exempt and Caps).
	// A difference in a tool's Effect or its own gate is reported but changes no digest.
	Covered bool
}

// Diff is the changes from old to new, in a fixed order, and an error when the two documents do not agree
// with their digests: different digests with no covered difference means the digest covers something the
// documents do not carry; equal digests with a covered difference means the digest does not cover
// something the documents do. Either way the report cannot be trusted, so it is not given.
func Diff(old, new Document) ([]Change, error) {
	for _, d := range []struct {
		name string
		doc  Document
	}{{"old", old}, {"new", new}} {
		if want := d.doc.Recompute(); d.doc.Digest != want {
			return nil, fmt.Errorf("the %s declarations' digest %q is not the digest of what they declare (%q)", d.name, d.doc.Digest, want)
		}
	}
	var out []Change
	out = append(out, diffPolicy(old.Policy, new.Policy)...)
	if old.Policy != nil && new.Policy != nil && old.Policy.RequireProvenance != new.Policy.RequireProvenance {
		if new.Policy.RequireProvenance {
			out = append(out, Change{Strengthened, "policy", "every tool must now declare a provenance and every run state its input's", true})
		} else {
			out = append(out, Change{Weakened, "policy", "tools and runs are no longer required to declare a provenance", true})
		}
	}
	oldTools, newTools := toolsByName(old), toolsByName(new)
	names := slices.Sorted(maps.Keys(func() map[string]bool {
		m := map[string]bool{}
		for n := range oldTools {
			m[n] = true
		}
		for n := range newTools {
			m[n] = true
		}
		return m
	}()))
	// A dump from before the idempotent member says nothing about it: its absence is not "false". When the two
	// documents disagree about carrying it and a tool is Idempotent in either, nothing can be compared; the
	// diff says so once and fails closed, rather than report every Idempotent tool as newly weakened.
	idemKnown := old.TracksIdempotent == new.TracksIdempotent
	if !idemKnown && slices.ContainsFunc(append(slices.Clone(old.Tools), new.Tools...), func(t Tool) bool { return t.Idempotent }) {
		out = append(out, Change{Weakened, "declarations", "a dump predates the idempotent member, so whether a tool is Idempotent cannot be compared; regenerate the baseline with this version", false})
	}
	for _, n := range names {
		out = append(out, diffTool(n, oldTools[n], newTools[n], idemKnown)...)
	}
	if err := checkDigests(old.Digest, new.Digest, out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkDigests holds the digests and the covered changes to each other: a difference in one and not the
// other means the digest and the declarations do not describe the same thing.
func checkDigests(oldDigest, newDigest string, changes []Change) error {
	if oldDigest == "" && newDigest == "" {
		return nil // no policy on either side: nothing is digested
	}
	covered := slices.ContainsFunc(changes, func(c Change) bool { return c.Covered })
	differ := oldDigest != newDigest
	switch {
	case differ && !covered:
		return fmt.Errorf("the digests differ (%.12s, %.12s) and the declarations show no difference: the digest covers something the declarations do not carry", oldDigest, newDigest)
	case !differ && covered:
		return fmt.Errorf("the digests are equal (%.12s) and the declarations differ in what the digest covers: the declarations carry something the digest does not cover", oldDigest)
	}
	return nil
}

func toolsByName(d Document) map[string]*Tool {
	m := make(map[string]*Tool, len(d.Tools))
	for i := range d.Tools {
		m[d.Tools[i].Name] = &d.Tools[i]
	}
	return m
}

// mode orders what a policy does to a tainted turn's side effect: nothing, ask the gate, refuse.
func mode(p *Policy) int {
	switch {
	case p == nil:
		return 0
	case p.Refuse:
		return 2
	case p.Gate != nil:
		return 1
	}
	return 0
}

func modeName(m int) string { return [...]string{"no policy", "an escalation gate", "refuse"}[m] }

func diffPolicy(old, new *Policy) []Change {
	const subject = "policy"
	om, nm := mode(old), mode(new)
	switch {
	case old == nil && new == nil:
		return nil
	case old == nil:
		return []Change{{Strengthened, subject, "a taint policy is added", true}}
	case new == nil:
		return []Change{{Weakened, subject, "the taint policy is removed", true}}
	case om != nm:
		class := Strengthened
		if nm < om {
			class = Weakened
		}
		return []Change{{class, subject, fmt.Sprintf("a tainted turn's side effect is %s, was %s", modeName(nm), modeName(om)), true}}
	case nm == 2:
		if !sameGate(old.Gate, new.Gate) {
			return []Change{{Changed, subject, "the gate changed (unused while the policy refuses)", true}}
		}
		return nil
	}
	return diffGate(subject, old.Gate, new.Gate, true)
}

func sameGate(a, b *Gate) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Single == b.Single && a.Need == b.Need && a.ExcludePrincipal == b.ExcludePrincipal && a.TimeoutMS == b.TimeoutMS &&
		sameSet(a.Approvers, b.Approvers)
}

func sameSet(a, b []string) bool {
	x, y := slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b))
	return slices.Equal(x, y)
}

// diffGate classifies the change of a gate: a lower need, a gate that no longer excludes the requester and
// an m-of-n gate turned into one decision are weaker. A changed approver set and a different timeout are
// changes: a rotated set is no weaker for being another, and a timeout moves when a wait expires, not what
// is required.
func diffGate(subject string, old, new *Gate, covered bool) []Change {
	switch {
	case old == nil && new == nil:
		return nil
	case old == nil:
		return []Change{{Strengthened, subject, "a gate is added", covered}}
	case new == nil:
		return []Change{{Weakened, subject, "the gate is removed", covered}}
	}
	var out []Change
	add := func(c Class, what string) { out = append(out, Change{c, subject, what, covered}) }
	switch {
	case old.Single && !new.Single:
		add(Strengthened, "one decision becomes an m-of-n gate")
	case !old.Single && new.Single:
		add(Weakened, "an m-of-n gate becomes one decision")
	case !old.Single:
		switch {
		case new.Need < old.Need:
			add(Weakened, fmt.Sprintf("need lowered from %d to %d", old.Need, new.Need))
		case new.Need > old.Need:
			add(Strengthened, fmt.Sprintf("need raised from %d to %d", old.Need, new.Need))
		}
		switch {
		case old.ExcludePrincipal && !new.ExcludePrincipal:
			add(Weakened, "the requester is no longer excluded")
		case !old.ExcludePrincipal && new.ExcludePrincipal:
			add(Strengthened, "the requester is now excluded")
		}
		if sameSet(old.Approvers, new.Approvers) && !slices.Equal(old.Approvers, new.Approvers) {
			// The same approvers in another order: no weaker, but the digest hashes the list in order, so it
			// differs.
			add(Changed, "the approver order changed (the same approvers; the order is part of the digest)")
		}
		if !sameSet(old.Approvers, new.Approvers) {
			removed := !isSubset(old.Approvers, new.Approvers)
			note := ""
			if !slices.ContainsFunc(new.Approvers, func(a string) bool { return slices.Contains(old.Approvers, a) }) {
				note = ", none in common with the old set"
			}
			if removed {
				// A rotation is a change a person must approve, even when need is still met.
				add(Weakened, fmt.Sprintf("an approver was removed or replaced (%d approvers, was %d%s): a person must approve a rotation", len(new.Approvers), len(old.Approvers), note))
			} else {
				add(Strengthened, fmt.Sprintf("approvers were added (%d approvers, was %d)", len(new.Approvers), len(old.Approvers)))
			}
		}
	}
	if old.TimeoutMS != new.TimeoutMS {
		add(Changed, fmt.Sprintf("the timeout changed (%s, was %s)", timeout(new.TimeoutMS), timeout(old.TimeoutMS)))
	}
	return out
}

func timeout(ms int64) string {
	if ms == 0 {
		return "none"
	}
	return fmt.Sprintf("%dms", ms)
}

func diffTool(name string, old, new *Tool, idemKnown bool) []Change {
	subject := "tool " + name
	var out []Change
	add := func(c Class, what string, covered bool) { out = append(out, Change{c, subject, what, covered}) }
	switch {
	case old == nil:
		switch {
		case new.Exempt != "":
			add(Weakened, fmt.Sprintf("added with TaintExempt (%q)", new.Exempt), true)
		case new.RaisesSafety != "":
			add(Weakened, fmt.Sprintf("added with RaisesSafety (%q)", new.RaisesSafety), true)
		case new.Untrusted != "" || len(new.Caps) > 0:
			add(Strengthened, "added with a declaration (untrusted source or caps)", true)
		default:
			add(Changed, "added", false)
		}
		return out
	case new == nil:
		switch {
		case old.Untrusted != "":
			add(Weakened, fmt.Sprintf("removed; it declared untrusted source %q", old.Untrusted), true)
		case len(old.Caps) > 0:
			add(Weakened, "removed; it carried cap rules", true)
		case old.Exempt != "":
			add(Strengthened, "removed; it was TaintExempt", true)
		case old.RaisesSafety != "":
			add(Strengthened, "removed; it was RaisesSafety", true)
		default:
			add(Changed, "removed", false)
		}
		return out
	}
	switch {
	case old.Untrusted != "" && new.Untrusted == "":
		add(Weakened, fmt.Sprintf("Untrusted(%q) removed", old.Untrusted), true)
	case old.Untrusted == "" && new.Untrusted != "":
		add(Strengthened, fmt.Sprintf("Untrusted(%q) added", new.Untrusted), true)
	case old.Untrusted != new.Untrusted:
		add(Changed, fmt.Sprintf("untrusted source %q, was %q", new.Untrusted, old.Untrusted), true)
	}
	switch {
	case old.Trusted == "" && new.Trusted != "":
		add(Changed, fmt.Sprintf("a trusted output is declared (%q)", new.Trusted), true)
	case old.Trusted != "" && new.Trusted == "":
		add(Changed, "the trusted output declaration is removed", true)
	case old.Trusted != new.Trusted:
		add(Changed, fmt.Sprintf("the trusted output reason is %q, was %q", new.Trusted, old.Trusted), true)
	}
	switch {
	case old.Exempt == "" && new.Exempt != "":
		add(Weakened, fmt.Sprintf("TaintExempt(%q) added", new.Exempt), true)
	case old.Exempt != "" && new.Exempt == "":
		add(Strengthened, "TaintExempt removed", true)
	case old.Exempt != new.Exempt:
		add(Weakened, fmt.Sprintf("TaintExempt reason changed to %q, was %q", new.Exempt, old.Exempt), true)
	}
	switch {
	case old.RaisesSafety == "" && new.RaisesSafety != "":
		add(Weakened, fmt.Sprintf("RaisesSafety(%q) added", new.RaisesSafety), true)
	case old.RaisesSafety != "" && new.RaisesSafety == "":
		add(Strengthened, "RaisesSafety removed", true)
	case old.RaisesSafety != new.RaisesSafety:
		add(Changed, fmt.Sprintf("the RaisesSafety reason is %q, was %q", new.RaisesSafety, old.RaisesSafety), true)
	}
	out = append(out, diffCaps(subject, old.Caps, new.Caps)...)
	// A move between side effect and Idempotent (the tool has side effects on both sides; a move to or from
	// ReadOnly is the Effect row below). Idempotent is retry-safe, so a resume re-runs the call.
	if idemKnown && old.Effect && new.Effect && old.Idempotent != new.Idempotent {
		if new.Idempotent {
			add(Weakened, "now Idempotent: a resume re-runs its calls instead of halting on an unknown outcome", false)
		} else {
			add(Strengthened, "no longer Idempotent: a resume halts on an unknown outcome instead of re-running its calls", false)
		}
	}
	if old.Effect != new.Effect {
		if new.Effect {
			add(Strengthened, "now has side effects: taint gates its calls", false)
		} else {
			add(Weakened, "no longer has side effects (ReadOnly): taint no longer gates its calls", false)
		}
	}
	out = append(out, diffGate(subject+" own gate", old.OwnGate, new.OwnGate, false)...)
	return out
}

// diffCaps compares two tools' cap lines through the grammar, not as text. A line that does not parse
// is a change the report cannot classify.
func diffCaps(subject string, oldLines, newLines []string) []Change {
	if slices.Equal(oldLines, newLines) {
		return nil
	}
	oldRules, errOld := amount.ParseRules(oldLines...)
	newRules, errNew := amount.ParseRules(newLines...)
	switch {
	case errNew != nil:
		// The new lines cannot be read, so nothing says they still limit anything: fail closed.
		return []Change{{Weakened, subject, "the new cap lines do not parse, so they cannot be compared; treated as weakened", true}}
	case errOld != nil:
		return []Change{{Weakened, subject, "the old cap lines do not parse, so the new ones cannot be compared; treated as weakened", true}}
	}
	var out []Change
	add := func(c Class, what string) { out = append(out, Change{c, subject, what, true}) }
	byName := func(rs []amount.Rule) map[string]amount.Rule {
		m := map[string]amount.Rule{}
		for _, r := range rs {
			m[r.Name] = r
		}
		return m
	}
	om, nm := byName(oldRules), byName(newRules)
	var removed, added []amount.Rule
	for _, r := range oldRules {
		if _, ok := nm[r.Name]; !ok {
			removed = append(removed, r)
		}
	}
	for _, r := range newRules {
		if _, ok := om[r.Name]; !ok {
			added = append(added, r)
		}
	}
	// A rule that is gone and another that is the same under a new name is a rename: a cumulative rule's
	// name is its ledger, so the window starts again from zero.
	for _, r := range slices.Clone(removed) {
		i := slices.IndexFunc(added, func(a amount.Rule) bool { return sameRuleBesidesName(r, a) })
		if i < 0 {
			continue
		}
		if r.Kind == amount.Sum {
			add(Weakened, fmt.Sprintf("cap rule %q renamed to %q: the cumulative window starts again", r.Name, added[i].Name))
		} else {
			add(Changed, fmt.Sprintf("cap rule %q renamed to %q", r.Name, added[i].Name))
		}
		removed = slices.DeleteFunc(removed, func(x amount.Rule) bool { return x.Name == r.Name })
		added = slices.Delete(added, i, i+1)
	}
	for _, r := range removed {
		add(Weakened, fmt.Sprintf("cap rule %q removed", r.Name))
	}
	for _, r := range added {
		add(Strengthened, fmt.Sprintf("cap rule %q added", r.Name))
	}
	for _, o := range oldRules {
		if n, ok := nm[o.Name]; ok {
			out = append(out, diffRule(subject, o, n)...)
		}
	}
	return out
}

func sameRuleBesidesName(a, b amount.Rule) bool {
	a.Name, b.Name = "", ""
	return ruleKey(a) == ruleKey(b)
}

func ruleKey(r amount.Rule) string {
	return fmt.Sprintf("%q|%q|%v|%q|%q|%v|%v|%d|%v|%q|%+v|%q", r.Tools, r.Arg, r.Unit, r.CurrencyArg, r.Currency, r.Exponent, r.Kind, r.Limit, r.Window, keyString(r.Key), r.Action, r.Name)
}

// diffRule classifies the differences between two rules of one name.
func diffRule(subject string, o, n amount.Rule) []Change {
	var out []Change
	add := func(c Class, what string) {
		out = append(out, Change{c, subject, fmt.Sprintf("cap rule %q: %s", o.Name, what), true})
	}
	if o.Kind != n.Kind {
		// A limit on one call and a limit on a total are not comparable, and a total can be far larger than the
		// call it replaces: reported as weakened, so a pipeline stops and a person reads it.
		add(Weakened, "the kind changed (per call and cumulative): the limits cannot be compared, treated as weakened")
		return out
	}
	switch {
	case o.Currency == n.Currency && o.Exponent == n.Exponent:
		switch {
		case n.Limit > o.Limit:
			add(Weakened, fmt.Sprintf("limit raised from %d to %d minor units", o.Limit, n.Limit))
		case n.Limit < o.Limit:
			add(Strengthened, fmt.Sprintf("limit lowered from %d to %d minor units", o.Limit, n.Limit))
		}
	default:
		// Minor units of two currencies (or two exponents) are not comparable, so a raised limit would hide
		// behind the swap: treated as weakened.
		add(Weakened, fmt.Sprintf("currency %s (exponent %d), was %s (exponent %d): the limits cannot be compared, treated as weakened", n.Currency, n.Exponent, o.Currency, o.Exponent))
	}
	if o.Action != n.Action {
		switch {
		case o.Action.Kind == amount.Refuse && n.Action.Kind == amount.Gate:
			add(Weakened, "refuse becomes a gate")
		case o.Action.Kind == amount.Gate && n.Action.Kind == amount.Refuse:
			add(Strengthened, "a gate becomes refuse")
		default:
			add(Changed, "the action changed")
		}
	}
	if o.Kind == amount.Sum && o.Window != n.Window {
		switch {
		case o.Window == amount.Hour && n.Window == amount.Day:
			add(Weakened, "the window is a day, was an hour")
		case o.Window == amount.Day && n.Window == amount.Hour:
			add(Strengthened, "the window is an hour, was a day")
		default:
			add(Changed, fmt.Sprintf("the window is %s, was %s", n.Window, o.Window))
		}
	}
	if !slices.Equal(o.Tools, n.Tools) {
		switch {
		case isSubset(n.Tools, o.Tools) && len(n.Tools) < len(o.Tools):
			add(Weakened, "applies to fewer tools")
		case isSubset(o.Tools, n.Tools) && len(n.Tools) > len(o.Tools):
			add(Strengthened, "applies to more tools")
		default:
			add(Changed, "the tools it applies to changed")
		}
	}
	if !slices.Equal(o.Arg, n.Arg) || o.Unit != n.Unit || !slices.Equal(o.CurrencyArg, n.CurrencyArg) {
		add(Changed, "the amount or currency member changed")
	}
	if keyString(o.Key) != keyString(n.Key) && o.Kind == amount.Sum {
		switch {
		case o.Key == nil:
			add(Weakened, "a key is added: one shared total becomes one total per key")
		case n.Key == nil:
			add(Strengthened, "the key is removed: one total per key becomes one shared total")
		default:
			add(Weakened, "the key changed: the totals cannot be compared, treated as weakened")
		}
	}
	return out
}

func isSubset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

func keyString(k *amount.Key) string {
	if k == nil {
		return "none"
	}
	return fmt.Sprintf("%+v/%q", *k, k.Path)
}
