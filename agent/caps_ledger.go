package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bide-ai/bide/internal/amount"
	"github.com/bide-ai/bide/internal/capledger"
)

// Cumulative caps (docs/design/untrusted-content-and-amount-caps.md, section 6.5): the rules of the
// form "sum N CUR per run|hour|day [by key] -> refuse" count the amounts of the calls the engine lets
// begin in the cap ledger (internal/capledger, model 17 in spec/tla/caps). The call site is the tool
// handler: after the call's attempt claim (the loop), after the kill-switch check, after the per-call
// rules, before the call begins. A reservation that would take the window over its limit refuses the
// call (a recorded known failure, the tool never called); a store that cannot be read or written fails
// the call before it is begun (an argsJournalError: nothing is recorded, the run fails, a re-drive
// reaches the call again), so a cap is fail closed.

// defaultCapNamespace names the ledgers when the host gives no WithCapNamespace.
const defaultCapNamespace = "default"

// capAttempt says which attempt of a call a tool handler serves: marked is true for a tool that is not
// retry-safe, whose attempt marker (id is its key) is claimed exclusively; an Idempotent tool has no
// marker, and its attempt is the tool-use ID alone (id is empty).
type capAttempt struct {
	marked bool
	id     string
}

type capAttemptKey struct{}

func withCapAttempt(ctx context.Context, at capAttempt) context.Context {
	return context.WithValue(ctx, capAttemptKey{}, at)
}

func capAttemptFrom(ctx context.Context) capAttempt {
	at, _ := ctx.Value(capAttemptKey{}).(capAttempt)
	return at
}

// capDemand is one cumulative rule's claim on a window for one call.
type capDemand struct {
	rule   amount.Rule
	amount int64
	key    string // the normalized key; "" for an unkeyed total
}

// demands evaluates the tool's cumulative rules on the call's arguments: the amounts to reserve, or the
// name of the rule that makes the call not assessable (an absent, negative, unparseable or non-numeric
// amount, a currency no rule of the argument covers, a missing or non-ASCII key, arguments that are not
// strict JSON). An amount of zero reserves nothing and is no demand.
func (tc *toolCaps) demands(args []byte, principal string) (ds []capDemand, bad string) {
	for _, g := range tc.groups {
		if !g.sum {
			continue
		}
		matched := g.rules
		if g.currency != nil {
			cur, err := callCurrency(args, g.currency)
			if err != nil {
				return nil, tc.strictest(g, capNotAssessable, "").Rule
			}
			matched = nil
			for _, i := range g.rules {
				if tc.rules[i].Currency == cur {
					matched = append(matched, i)
				}
			}
			if len(matched) == 0 {
				return nil, tc.strictest(g, capCurrency, cur).Rule
			}
		}
		raw, lerr := amount.Lookup(args, g.arg)
		for _, i := range matched {
			r := tc.rules[i]
			if lerr != nil {
				return nil, r.Name
			}
			v, err := amount.Assess(raw, r.Unit, r.Exponent)
			if err != nil {
				return nil, r.Name
			}
			if v == 0 {
				continue
			}
			key, ok := capKey(args, r.Key, principal)
			if !ok {
				return nil, r.Name
			}
			ds = append(ds, capDemand{rule: r, amount: v, key: key})
		}
	}
	return ds, ""
}

// capKey is a rule's key for a call, normalized as the rule says (lower, trim): the empty string for an
// unkeyed total. ok is false when the key cannot be assessed: the member is absent, not a string or an
// integer, empty after normalization, or holds a byte outside ASCII (the ledger's key is a digest of
// the text, and without Unicode normalization two spellings of one customer would be two keys, so a
// non-ASCII key is refused rather than split a customer's window).
func capKey(args []byte, k *amount.Key, principal string) (string, bool) {
	if k == nil {
		return "", true
	}
	var text string
	if k.Principal {
		text = principal
	} else {
		raw, err := amount.Lookup(args, k.Path)
		if err != nil || len(raw) == 0 {
			return "", false
		}
		if raw[0] == '"' {
			if err := json.Unmarshal(raw, &text); err != nil {
				return "", false
			}
		} else if raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9' {
			text = string(raw)
		} else {
			return "", false
		}
	}
	if k.Trim {
		text = strings.TrimSpace(text)
	}
	if k.Lower {
		text = strings.ToLower(text)
	}
	if text == "" {
		return "", false
	}
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 || text[i] < 0x20 {
			return "", false
		}
	}
	return text, true
}

// reserveCaps is the enforcement point of the cumulative rules: it reserves every demand of the call in
// its window's ledger. An amount over a limit refuses the call (errCapRefused); a call that is not
// assessable is refused too; a store error is an argsJournalError, so the tool is not begun. A tool with
// no cumulative rule is not asked.
func (a *Agent) reserveCaps(ctx context.Context, runID string, tu ToolUse, spec *ToolSpec) error {
	tc := a.caps[tu.Name]
	if tc == nil || !tc.hasSum() {
		return nil
	}
	ds, bad := tc.demands(tu.Args, principalOf(ctx))
	if bad != "" {
		return &errCapRefused{rule: bad}
	}
	at := capAttemptFrom(ctx)
	root := rootRunID(ctx, runID)
	ns := a.capNamespace
	if ns == "" {
		ns = defaultCapNamespace
	}
	for _, d := range ds {
		kind, err := ledgerKind(d.rule.Window)
		if err != nil {
			return &argsJournalError{err}
		}
		// The attempt is the marker's key for a tool with markers, so a re-attempt after a not-started
		// reserves anew, and the tool-use ID alone for a retry-safe one; the rule's name is part of it,
		// so a call under two rules holds a slot (and an intent) in each ledger.
		attemptID := at.id
		if !at.marked {
			attemptID = "idem"
		}
		req := capledger.Request{
			Rule:      capledger.Rule{Namespace: ns, Name: d.rule.Name, Currency: d.rule.Currency, Kind: kind},
			KeyDigest: capledger.KeyDigest(d.key),
			RunID:     runID, RootRunID: root,
			ToolUseID: tu.ID, AttemptID: attemptID + "#" + d.rule.Name,
			Marked: at.marked, Amount: d.amount, Limit: d.rule.Limit,
			Clock: clockFrom(ctx),
		}
		if _, err := a.capLedger.Reserve(ctx, req); err != nil {
			if errors.Is(err, capledger.ErrOverLimit) {
				return &errCapRefused{rule: d.rule.Name}
			}
			return &argsJournalError{fmt.Errorf("cap ledger: reserve for rule %q: %w (%w)", cutName(d.rule.Name), err, ErrStorage)}
		}
	}
	return nil
}

func ledgerKind(w amount.Window) (capledger.Kind, error) {
	switch w {
	case amount.Run:
		return capledger.Run, nil
	case amount.Hour:
		return capledger.Hour, nil
	case amount.Day:
		return capledger.Day, nil
	}
	return "", fmt.Errorf("cap ledger: unknown window %v", w)
}

func (tc *toolCaps) hasSum() bool {
	for _, g := range tc.groups {
		if g.sum {
			return true
		}
	}
	return false
}

// principalOf is the principal of the run ctx drives, for a rule "by principal": the run's own, the last of
// the chain its drive bound. It is empty when the run has none, which makes the call not assessable.
func principalOf(ctx context.Context) string {
	chain := chainFrom(ctx)
	if len(chain) == 0 {
		return ""
	}
	return chain[len(chain)-1].OnBehalfOf
}
