package agent

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
func (a *Agent) reserveCaps(ctx context.Context, runID string, call ToolCall, spec *ToolSpec) error {
	tu := call.Use
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
		attemptID := at.id // empty for a tool with no marker: the tool-use ID and the rule name are the attempt
		req := capledger.Request{
			Rule:      capledger.Rule{Namespace: ns, Name: d.rule.Name, Currency: d.rule.Currency, Kind: kind},
			KeyDigest: capledger.KeyDigest(d.key),
			RunID:     runID, RootRunID: root,
			ToolUseID: tu.ID, AttemptID: attemptID + "#" + d.rule.Name,
			Marked: at.marked, Amount: d.amount, Limit: d.rule.Limit,
			Clock: clockFrom(ctx),
		}
		_, err = a.capLedger.Reserve(ctx, req)
		if err == nil && call.capHeld != nil {
			call.capHeld.add(capledger.Target{Rule: req.Rule, KeyDigest: req.KeyDigest, RunID: req.RunID, RootRunID: req.RootRunID,
				ToolUseID: req.ToolUseID, AttemptID: req.AttemptID}, req.Marked)
		}
		if err != nil {
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

// capReleaseFailures counts the releases the loop could not write in this process (each left its slot counted
// for the reconcile pass); there is no metrics hook in the engine, so tests read it and the log line carries it.
var capReleaseFailures atomic.Int64

// capMarkerKey is the attempt marker key inside a slot's Att, which the reservation wrote as
// "<marker key>#<rule name>": everything before the last '#' (a rule name is letters, digits, '_' and '-',
// so the last '#' is always the separator, whatever a marker key holds). An unmarked attempt has no marker
// key: ok is false.
func capMarkerKey(att string) (string, bool) {
	i := strings.LastIndexByte(att, '#')
	if i <= 0 {
		return "", false
	}
	return att[:i], true
}

// capHeld is the reservations one call made, by the handlers of one chain: the targets the loop releases
// when it proves the tool never began. A handler that reserves after the loop took the list leaves its
// slot to the reconcile pass.
type capHeld struct {
	mu      sync.Mutex
	targets []capledger.Target
	marked  []bool
	taken   bool
}

func (h *capHeld) add(t capledger.Target, marked bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.taken {
		return // the loop has released what it knew; this slot is a leak for reconcile
	}
	for _, x := range h.targets {
		if x == t {
			return
		}
	}
	h.targets = append(h.targets, t)
	h.marked = append(h.marked, marked)
}

// take returns the targets of marked reservations and stops the list from growing.
func (h *capHeld) take() []capledger.Target {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.taken = true
	var out []capledger.Target
	for i, t := range h.targets {
		if h.marked[i] {
			out = append(out, t)
		}
	}
	return out
}

// releaseCaps is the loop's release, called once the chain has returned and the call reached its tool
// handler: it seals the call's began word (one compare and swap; a tool that began gives no seal, so
// nothing is released) and releases each marked reservation of the call, before the attempt is voided.
// A release that fails leaves the slot counted, and the void then lets the reconcile pass release it.
// It reports whether it sealed the word. It is the only releaser: no handler writes a release.
// protocol:caps begin LoopSeal
func (a *Agent) releaseCaps(ctx context.Context, held *capHeld, began *atomic.Int32) (sealed bool) {
	targets := held.take()
	if len(targets) == 0 {
		return false
	}
	attempts := make([]string, len(targets))
	for i, t := range targets {
		attempts[i] = a.capLedger.AttemptKey(t.RunID, t.RootRunID, t.ToolUseID, t.AttemptID)
	}
	seals, ok := capledger.NewSeals(attempts, began, beganNone, beganSealed)
	if !ok {
		return false
	}
	for i, t := range targets {
		if _, err := a.capLedger.Release(context.WithoutCancel(ctx), t, seals[i]); err != nil {
			// The slot stays counted (the safe direction) and the void that follows lets the reconcile pass
			// release it; say so, without any amount.
			capReleaseFailures.Add(1)
			slog.Default().Warn("bide: a cap reservation could not be released; it stays counted until the reconcile pass",
				"run", t.RunID, "tool_use_id", t.ToolUseID, "rule", t.Rule.Name, "error", err.Error())
		}
	}
	return true
}

// protocol:caps end

// reconcileCaps is the reconcile pass over every cap ledger of the store (Recover runs it): it releases the
// live slots of marked attempts whose attempt marker the run's journal records as not started, which a
// void without a release left counted (a release that failed, a handler that reserved after the call was
// sealed). It acts on voided attempts only: a call that began, or that ResolveHalt resolved, or whose
// marker still stands, keeps its slot, and so does every unmarked attempt. It returns what it released.
func reconcileCaps(ctx context.Context, store *Journal) ([]string, error) {
	lister, ok := Capability[Lister](store.store)
	if !ok {
		return nil, nil
	}
	var runs []string
	for run, err := range lister.Runs(ctx, RunFilter{Prefix: capRunPrefix}) {
		if err != nil {
			return nil, fmt.Errorf("list the cap ledgers: %w (%w)", err, ErrStorage)
		}
		runs = append(runs, run)
	}
	return reconcileCapRuns(ctx, store, runs)
}

// capWindowOpen reports whether a window is worth reconciling, from the kind and id its attempts' cap-intent
// fixed (never parsed out of a run's name: a root run may be named like a date): a day or hour window whose
// end is less than one window length in the past, or a run window whose root run (window is its id) has not
// ended. A slot left counted in a closed window costs capacity only to an attempt that resumes in it (a
// resumed attempt keeps its window) or to a node with a lagging clock; skipping it is the over-count
// direction, and it refines the model's nondeterministic Reconcile action (every property holds under any
// subset of reconcile runs). The cost of a pass follows the open windows, not every ledger ever written. A
// store error is returned, never read as "closed"; an unknown kind is open.
func capWindowOpen(ctx context.Context, store *Journal, kind capledger.Kind, window string, now time.Time) (bool, error) {
	switch kind {
	case capledger.Day:
		t, err := time.Parse("2006-01-02", window)
		if err != nil {
			return true, nil
		}
		return now.Before(t.Add(48 * time.Hour)), nil
	case capledger.Hour:
		t, err := time.Parse("2006-01-02T15", window)
		if err != nil {
			return true, nil
		}
		return now.Before(t.Add(2 * time.Hour)), nil
	case capledger.Run:
		ended, err := capRootEnded(ctx, store, window)
		if err != nil {
			return false, err
		}
		return !ended, nil
	}
	return true, nil
}

// endedRoots remembers the root runs known to have ended, so a pass does not read their markers again: an
// ended run stays ended. It is bounded; the least recently used is dropped and read again if needed.
var endedRoots = &endedCache{max: 4096, set: map[string]*list.Element{}, order: list.New()}

type endedCache struct {
	mu    sync.Mutex
	max   int
	set   map[string]*list.Element
	order *list.List
}

func (c *endedCache) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.set[id]; ok {
		c.order.MoveToFront(e)
		return true
	}
	return false
}

func (c *endedCache) add(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.set[id]; ok {
		c.order.MoveToFront(e)
		return
	}
	c.set[id] = c.order.PushFront(id)
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.set, last.Value.(string))
	}
}

// capRootEnded reports whether root holds a terminal marker (it completed, aborted or was cancelled).
func capRootEnded(ctx context.Context, store *Journal, root string) (bool, error) {
	if endedRoots.has(storeKey(store, root)) {
		return true, nil
	}
	for _, marker := range endOfRunMarkers {
		if _, found, err := store.Get(ctx, root, marker); err != nil {
			return false, fmt.Errorf("read whether run %s ended: %w", root, err)
		} else if found {
			endedRoots.add(storeKey(store, root))
			return true, nil
		}
	}
	return false, nil
}

// storeKey names a run within a store, for the process-wide cache.
func storeKey(store *Journal, run string) string {
	id, _ := store.identity()
	return fmt.Sprintf("%v/%s", id, run)
}

// reconcileCapRuns is reconcileCaps over the cap runs a caller already listed (Recover finds them in its own
// listing of runs, which includes them: a ledger never ends). Index runs among them are skipped.
func reconcileCapRuns(ctx context.Context, store *Journal, runs []string) ([]string, error) {
	l := newCapLedger(store, time.Now, 0)
	var released []string
	var errs []error
	for _, run := range runs {
		if strings.Count(run, "/") != 5 {
			continue // an index run, not a ledger
		}
		live, err := l.Live(ctx, run)
		if err != nil {
			errs = append(errs, fmt.Errorf("read cap ledger %s: %w", run, err))
			continue
		}
		if len(live) == 0 {
			continue // nothing counted that a release could free
		}
		// The ledger's window, as its attempts' cap-intent fixed it (all slots of a ledger share one).
		if kind, window, ok, err := l.WindowOf(ctx, live[0]); err != nil {
			errs = append(errs, err)
			continue
		} else if ok {
			if open, err := capWindowOpen(ctx, store, kind, window, time.Now()); err != nil {
				errs = append(errs, err)
				continue
			} else if !open {
				continue
			}
		}
		got, err := l.Reconcile(ctx, run, func(ctx context.Context, s capledger.Slot) (bool, error) {
			key, found := capMarkerKey(s.Att)
			if !found {
				return false, nil // an unmarked attempt carries no marker key; it is never released
			}
			marker, ok, err := store.Get(ctx, s.Run, key)
			if err != nil || !ok || marker.Kind != StepAttempt {
				return false, err
			}
			return store.voided(ctx, s.Run, key, marker)
		})
		released = append(released, got...)
		if err != nil {
			errs = append(errs, fmt.Errorf("reconcile cap ledger %s: %w", run, err))
		}
	}
	return released, errors.Join(errs...)
}
