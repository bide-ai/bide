package runview

import (
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/bide-ai/bide/agent"
)

// ApproverCard is one approver of an approval gate, as the page shows them: who, what they decided,
// whether their signature verified, the fingerprint of their key and when it was signed.
type ApproverCard struct {
	Name     string
	Initials string
	// State is approved, denied or pending.
	State string
	// Verify says what is known of the signature, in words: "signature verified", "signature NOT
	// verified", "in the signed log, signature not checked" or "signature not checked". VerifyOK
	// reports the first. Empty for a pending approver.
	Verify   string
	VerifyOK bool
	Why      string // why the decision did not count, when it did not
	// Fingerprint is the approver's key as "ed25519:3b6a27bc" (the first 8 hex digits of the
	// SHA-256 of its public key), when -approver-keys gave it.
	Fingerprint string
	// SignedBy is when the signed head that covers the decision was signed: the decision was
	// journaled no later than that, since the journal records no time of its own for it.
	SignedBy time.Time
}

// initials is up to two letters standing for an approver id: the first letters of its words
// ("risk-lead" is RL), or its first two ("ops" is OP).
func initials(id string) string {
	words := strings.FieldsFunc(id, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var out []rune
	switch {
	case len(words) >= 2:
		for _, w := range words[:2] {
			out = append(out, []rune(w)[0])
		}
	case len(words) == 1:
		out = []rune(words[0])
		out = out[:min(2, len(out))]
	default:
		return "?"
	}
	return strings.ToUpper(string(out))
}

// fingerprints is each approver's key fingerprint under keys, the approvers that have a key only.
func fingerprints(approvers []string, keys agent.ApproverVerifierFor) map[string]string {
	if keys == nil {
		return nil
	}
	out := map[string]string{}
	for _, a := range approvers {
		v, ok := keys(a)
		if !ok {
			continue
		}
		var fps []string
		for _, id := range v.KeyIDs() {
			alg, hexKey, ok := strings.Cut(id, ":")
			if !ok {
				alg, hexKey = "", id
			}
			if len(hexKey) > 8 {
				hexKey = hexKey[:8]
			}
			if alg != "" {
				hexKey = alg + ":" + hexKey
			}
			fps = append(fps, hexKey)
		}
		if len(fps) > 0 {
			out[a] = strings.Join(fps, " + ")
		}
	}
	return out
}

// cards builds the gate's approver cards from its decisions and the proof's approval check, if
// there is one. A one-decision gate is not signed: it has none.
func cards(g Gate, p *Proof) []ApproverCard {
	if g.Single {
		return nil
	}
	chk, checked := p.approval(g.ToolUseID)
	var signedBy time.Time
	if p != nil {
		signedBy = p.SignedAt
	}
	names := slices.Clone(g.Approvers)
	for _, n := range slices.Concat(g.ApprovedBy, g.DeniedBy) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	var out []ApproverCard
	for _, n := range names {
		c := ApproverCard{Name: n, Initials: initials(n), Fingerprint: chk.Keys[n]}
		switch {
		case slices.Contains(g.ApprovedBy, n):
			c.State = "approved"
		case slices.Contains(g.DeniedBy, n):
			c.State = "denied"
		default:
			c.State = "pending"
		}
		if c.State != "pending" {
			c.SignedBy = signedBy
			switch {
			case checked && (slices.Contains(chk.Counted, n) || slices.Contains(chk.Denied, n)):
				c.Verify, c.VerifyOK = "signature verified", true
			case checked:
				c.Verify, c.Why = "signature NOT verified", chk.Ignored[n]
			case p != nil && p.OK:
				c.Verify = "in the signed log, signature not checked"
			default:
				c.Verify = "signature not checked"
			}
		}
		out = append(out, c)
	}
	return out
}
