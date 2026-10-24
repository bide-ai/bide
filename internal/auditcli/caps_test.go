package auditcli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/capbridge"
	"github.com/bide-ai/bide/internal/capledger"
	"github.com/bide-ai/bide/internal/journalhook"
)

// bide audit caps is wired into the verb table by its own commit; these tests call it directly.
func init() { verbs["caps"] = (*cli).caps }

const (
	capsRule   = `refund_total: refund.amount[minor, currency=currency] sum 100 USD per day -> refuse`
	capsRuleHr = `refund_hour: refund.amount[minor, currency=currency] sum 1 USD per hour -> refuse`
	capsDay    = "2026-10-22"
)

var capsNow = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

// tamperStore reads a MemStore the way an attacker who can write the database leaves it: entries
// hidden, or their bytes edited. The runs are written through the real store; only the audit reads this.
type tamperStore struct {
	*agent.MemStore
	hide func(run, name string) bool
	gone func(run string) bool // a run that is not in the store at all
	edit func(run, name string, data []byte) []byte
}

func (s *tamperStore) Load(ctx context.Context, run string, after int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) {
		for e, err := range s.MemStore.Load(ctx, run, after) {
			if err == nil && s.hide != nil && s.hide(run, e.Name) {
				continue
			}
			if err == nil && s.edit != nil {
				e.Data = s.edit(run, e.Name, e.Data)
			}
			if !yield(e, err) {
				return
			}
		}
	}
}

func (s *tamperStore) Runs(ctx context.Context, f agent.RunFilter) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		for id, err := range s.MemStore.Runs(ctx, f) {
			if err == nil && s.gone != nil && s.gone(id) {
				continue
			}
			if !yield(id, err) {
				return
			}
		}
	}
}

func (s *tamperStore) Get(ctx context.Context, run, name string) (agent.Entry, bool, error) {
	e, ok, err := s.MemStore.Get(ctx, run, name)
	if err != nil || !ok {
		return e, ok, err
	}
	if s.hide != nil && s.hide(run, name) {
		return agent.Entry{}, false, nil
	}
	if s.edit != nil {
		e.Data = s.edit(run, name, e.Data)
	}
	return e, true, nil
}

// capsEnv is a store with cap ledgers written through an AuditedStore, so heads are anchored.
type capsEnv struct {
	t      *testing.T
	mem    *agent.MemStore
	as     *audit.AuditedStore
	j      *agent.Journal
	anchor *audit.MemAnchorLog
	pub    string
	dir    string
	tamper *tamperStore
}

func newCapsEnv(t *testing.T) *capsEnv {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	mem := agent.NewMemStore()
	anchor := audit.NewMemAnchorLog()
	as, err := audit.NewAuditedStore(mem, audit.Ed25519Signer{Priv: priv}, anchor)
	if err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(as)
	if err != nil {
		t.Fatal(err)
	}
	e := &capsEnv{t: t, mem: mem, as: as, j: j, anchor: anchor, pub: hex.EncodeToString(pub), dir: t.TempDir(), tamper: &tamperStore{MemStore: mem}}
	t.Cleanup(func() { Stores = nil })
	Stores = &StoreOpeners{SQLite: func(string) (NotifyStores, error) {
		return NotifyStores{Journal: e.tamper, Events: audit.NewMemEventStore(), Close: func() error { return nil }}, nil
	}}
	return e
}

// call runs one refund of amount as run runID, under the given rule lines.
func (e *capsEnv) call(runID string, amount int, lines ...string) error {
	e.t.Helper()
	if len(lines) == 0 {
		lines = []string{capsRule}
	}
	type in struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	tool, err := agent.Func("refund", "refund", func(context.Context, in) (string, error) { return "ok", nil }, agent.WithCaps(lines...))
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", fmt.Sprintf(`{"amount":%d,"currency":"USD"}`, amount)), agenttest.TextTurn("done")),
		e.j, agent.WithTools(tool), agent.WithClock(func() time.Time { return capsNow }))
	if err != nil {
		e.t.Fatal(err)
	}
	_, err = a.Run(context.Background(), runID, agent.UserText("go"))
	return err
}

// ledgerRun is the one ledger run of the store.
func (e *capsEnv) ledgerRun() string {
	e.t.Helper()
	var out []string
	for id := range e.mem.Runs(context.Background(), agent.RunFilter{Prefix: "@cap/"}) {
		if strings.Count(id, "/") == 5 {
			out = append(out, id)
		}
	}
	if len(out) != 1 {
		e.t.Fatalf("ledger runs = %v, want one", out)
	}
	return out[0]
}

// heads writes every anchored head to a file, one JSON value a line, and returns its path.
func (e *capsEnv) heads() string {
	e.t.Helper()
	if err := e.as.Flush(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	var b bytes.Buffer
	for _, en := range e.anchor.Entries() {
		line, err := json.Marshal(en.STH)
		if err != nil {
			e.t.Fatal(err)
		}
		b.Write(append(line, '\n'))
	}
	p := filepath.Join(e.dir, "heads.jsonl")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *capsEnv) anchored(extra ...string) (int, string, string) {
	return runCLI(append([]string{"caps", "-db", "x.db", "-sth", e.heads(), "-pubkey", e.pub}, extra...)...)
}

func unanchoredCaps(extra ...string) (int, string, string) {
	return runCLI(append([]string{"caps", "-db", "x.db", "-unanchored"}, extra...)...)
}

// threeCalls is the store of most tests: three runs reserving 30, 50 and 20 in one day's ledger.
func threeCalls(t *testing.T) *capsEnv {
	t.Helper()
	e := newCapsEnv(t)
	for i, amt := range []int{30, 50, 20} {
		if err := e.call(fmt.Sprintf("r%d", i+1), amt); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// wantFail checks an exit 1 whose output names every kind of finding.
func wantFail(t *testing.T, code int, out, errOut string, kinds ...string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s\n%s", code, out, errOut)
	}
	for _, k := range kinds {
		if !strings.Contains(out, "FAIL  "+k+" ") {
			t.Fatalf("no %s finding:\n%s", k, out)
		}
	}
	if strings.Contains(out, "PASS") {
		t.Fatalf("a failed audit printed PASS:\n%s", out)
	}
}

func TestCapsAudit_CleanLedgers(t *testing.T) {
	e := threeCalls(t)
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "3 slot(s), 3 live, window total 100 USD") || !strings.Contains(out, "PASS: 1 ledger(s)") {
		t.Fatalf("unanchored: exit %d\n%s\n%s", code, out, errOut)
	}
	if first, _, _ := strings.Cut(out, "\n"); !strings.HasPrefix(first, "UNANCHORED: whoever can write the store can delete a slot or append a release undetected") ||
		!strings.Contains(first, "only finds inconsistencies it can see") {
		t.Fatalf("the first line of an unanchored report must state the limit, got %q", first)
	}
	if !strings.Contains(out, "(UNANCHORED)") {
		t.Fatalf("the PASS line of an unanchored audit must say so:\n%s", out)
	}
	code, out, errOut = e.anchored()
	if code != 0 || !strings.Contains(out, "and is attested by a signed head") || strings.Contains(out, "UNANCHORED") {
		t.Fatalf("anchored: exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_SlotDeleted(t *testing.T) {
	e := threeCalls(t)
	run := e.ledgerRun()
	heads := e.heads()
	e.tamper.hide = func(r, n string) bool { return r == run && n == "slot:2" }
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "DELETED")
	if !strings.Contains(out, "records a reservation in slot 2, and the ledger holds 2 slot(s)") {
		t.Fatalf("DELETED must name the slot:\n%s", out)
	}
	// Anchored, the head the store signed over three slots no longer matches either.
	code, out, errOut = runCLI("caps", "-db", "x.db", "-sth", heads, "-pubkey", e.pub)
	wantFail(t, code, out, errOut, "DELETED", "DIVERGENT_HEAD")
}

func TestCapsAudit_SlotChanged(t *testing.T) {
	e := threeCalls(t)
	run := e.ledgerRun()
	// The last slot is rewritten to 10 with a total that still follows (the ledger's own rules accept it).
	e.tamper.edit = func(r, n string, d []byte) []byte {
		if r == run && n == "slot:2" {
			d = bytes.Replace(d, []byte(`"amount":20`), []byte(`"amount":10`), 1)
			d = bytes.Replace(d, []byte(`"cum_after":100`), []byte(`"cum_after":90`), 1)
		}
		return d
	}
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "CHANGED")
	if !strings.Contains(out, "records cum_after 100 for slot 2, and the ledger holds 90") {
		t.Fatalf("CHANGED must say what differs:\n%s", out)
	}
	if code, out, errOut = e.anchored(); true {
		wantFail(t, code, out, errOut, "CHANGED", "DIVERGENT_HEAD")
	}
}

func TestCapsAudit_SlotBrokenOutsideTheLedgersRules(t *testing.T) {
	e := threeCalls(t)
	run := e.ledgerRun()
	e.tamper.edit = func(r, n string, d []byte) []byte {
		if r == run && n == "slot:1" {
			d = bytes.Replace(d, []byte(`"cum_after":80`), []byte(`"cum_after":10`), 1)
		}
		return d
	}
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "CORRUPT")
}

func TestCapsAudit_SlotInsertedWithNoRecord(t *testing.T) {
	e := threeCalls(t)
	// A fourth reservation by a writer that is not the loop: its slot is in the ledger, and the run that
	// made the call holds no cap-reserve record of it.
	if err := e.call("r4", 5); err != nil {
		t.Fatal(err)
	}
	e.tamper.hide = func(r, n string) bool { return r == "r4" && strings.HasPrefix(n, "cap-reserve:") }
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "UNRECORDED")
	if !strings.Contains(out, "UNRECORDED") || !strings.Contains(out, "slot 3 (attempt r4/r4/c1/") {
		t.Fatalf("UNRECORDED must name slot 3:\n%s", out)
	}
}

func TestCapsAudit_CapReserveDeleted(t *testing.T) {
	e := threeCalls(t)
	e.tamper.hide = func(r, n string) bool { return r == "r2" && strings.HasPrefix(n, "cap-reserve:") }
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "UNRECORDED")
	if !strings.Contains(out, "slot 1 (attempt r2/") {
		t.Fatalf("the run whose record is gone is slot 1's:\n%s", out)
	}
}

func TestCapsAudit_LedgerDeleted(t *testing.T) {
	e := threeCalls(t)
	run := e.ledgerRun()
	heads := e.heads()
	e.tamper.gone = func(r string) bool { return r == run }
	code, out, errOut := unanchoredCaps()
	// The ledger run is gone from the store, but the runs that reserved in it still record it.
	wantFail(t, code, out, errOut, "LEDGER_MISSING")
	code, out, errOut = runCLI("caps", "-db", "x.db", "-sth", heads, "-pubkey", e.pub)
	wantFail(t, code, out, errOut, "LEDGER_MISSING")
	if !strings.Contains(out, "a signed head covers this ledger, and the store holds no such ledger") {
		t.Fatalf("the signed head of a deleted ledger must be named:\n%s", out)
	}
}

func TestCapsAudit_ReleaseAppendedAfterTheNewestHead(t *testing.T) {
	e := threeCalls(t)
	heads := e.heads()
	// A refusal by a second rule releases its first slot: a release is appended after the head.
	_ = e.call("r4", 150, capsRule, capsRuleHr) // the tool error is the model's to read; the run ends
	// Heads were taken before the release: the ledger now has entries no head covers.
	code, out, errOut := runCLI("caps", "-db", "x.db", "-sth", heads, "-pubkey", e.pub)
	wantFail(t, code, out, errOut, "NOT_ATTESTED")
	if !strings.Contains(out, "are NOT ATTESTED") {
		t.Fatalf("the tail must be named:\n%s", out)
	}
	// Unanchored, nothing can see it: the audit passes, and says why that is not a clean bill.
	code, out, errOut = unanchoredCaps()
	if code != 0 || !strings.HasPrefix(out, "UNANCHORED:") {
		t.Fatalf("unanchored: exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_UsageErrors(t *testing.T) {
	e := threeCalls(t)
	h := e.heads()
	for name, args := range map[string][]string{
		"neither -sth nor -unanchored": {"caps", "-db", "x.db"},
		"both":                         {"caps", "-db", "x.db", "-sth", h, "-pubkey", e.pub, "-unanchored"},
		"-sth without -pubkey":         {"caps", "-db", "x.db", "-sth", h},
		"-pubkey without -sth":         {"caps", "-db", "x.db", "-pubkey", e.pub},
		"no store":                     {"caps", "-unanchored"},
		"two stores":                   {"caps", "-db", "x.db", "-postgres-env", "X", "-unanchored"},
	} {
		if code, out, errOut := runCLI(args...); code != 2 || strings.Contains(out, "PASS") {
			t.Fatalf("%s: exit %d, want 2\n%s\n%s", name, code, out, errOut)
		}
	}
}

func TestCapsAudit_FiltersSayWhatTheyDidNotCheck(t *testing.T) {
	e := threeCalls(t)
	// A second ledger: another rule.
	if err := e.call("r5", 7, `refund_other: refund.amount[minor, currency=currency] sum 100 USD per day -> refuse`); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"-rule":      {"-rule", "refund_total"},
		"-namespace": {"-namespace", "default"},
		"-window":    {"-window", capsDay},
		"-run":       {"-run", "r2"},
	} {
		code, out, errOut := unanchoredCaps(args...)
		if code != 0 || !strings.Contains(out, "PASS (filtered)") || !strings.Contains(out, "NOT CHECKED") || strings.Contains(out, "PASS: ") {
			t.Fatalf("%s: exit %d\n%s\n%s", name, code, out, errOut)
		}
	}
	_, out, _ := unanchoredCaps("-rule", "refund_total")
	if strings.Count(out, "INFO  ledger ") != 1 || strings.Contains(out, "refund_other") {
		t.Fatalf("-rule must read one ledger:\n%s", out)
	}
	_, out, _ = unanchoredCaps("-run", "r2")
	if strings.Count(out, "INFO  ledger ") != 1 || !strings.Contains(out, "-run: ledgers with no slot or record of this run were not checked") {
		t.Fatalf("-run r2 touches one ledger and says what else it did not check:\n%s", out)
	}
	// An unfiltered audit makes no such statement.
	if _, out, _ := unanchoredCaps(); strings.Contains(out, "NOT CHECKED") || strings.Contains(out, "(filtered)") {
		t.Fatalf("an unfiltered audit checked everything:\n%s", out)
	}
}

func TestCapsAudit_FilteredRunStillSeesItsOwnDamage(t *testing.T) {
	e := threeCalls(t)
	e.tamper.hide = func(r, n string) bool { return r == "r2" && strings.HasPrefix(n, "cap-reserve:") }
	code, out, errOut := unanchoredCaps("-run", "r2")
	wantFail(t, code, out, errOut, "UNRECORDED")
	// Damage in another run's record is outside -run, and the report says so.
	code, out, errOut = unanchoredCaps("-run", "r1")
	if code != 0 || !strings.Contains(out, "NOT CHECKED") {
		t.Fatalf("-run r1: exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_JSON(t *testing.T) {
	e := threeCalls(t)
	run := e.ledgerRun()
	code, out, errOut := unanchoredCaps("-json")
	var rep struct {
		ExitCode int      `json:"exit_code"`
		Verified bool     `json:"verified"`
		Verb     string   `json:"verb"`
		Output   []string `json:"output"`
		Artifact struct {
			Anchored bool `json:"anchored"`
			Ledgers  []struct {
				Ledger string `json:"ledger"`
				Slots  int    `json:"slots"`
				Total  int64  `json:"total"`
			} `json:"ledgers"`
			Findings []struct {
				Kind string `json:"kind"`
			} `json:"findings"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s\n%s", err, out, errOut)
	}
	if code != 0 || !rep.Verified || rep.Verb != "caps" || rep.Artifact.Anchored || len(rep.Artifact.Ledgers) != 1 ||
		rep.Artifact.Ledgers[0].Ledger != run || rep.Artifact.Ledgers[0].Slots != 3 || rep.Artifact.Ledgers[0].Total != 100 || len(rep.Artifact.Findings) != 0 {
		t.Fatalf("exit %d, report %+v", code, rep)
	}
	if len(rep.Output) == 0 || !strings.HasPrefix(rep.Output[0], "UNANCHORED:") {
		t.Fatalf("the first output line of an unanchored report is the limit: %v", rep.Output)
	}
	e.tamper.hide = func(r, n string) bool { return r == run && n == "slot:2" }
	code, out, _ = unanchoredCaps("-json")
	rep.Artifact.Findings = nil
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 1 || rep.Verified || len(rep.Artifact.Findings) == 0 || rep.Artifact.Findings[0].Kind != "DELETED" {
		t.Fatalf("tampered: exit %d, err %v, report %+v", code, err, rep)
	}
}

func TestCapsAudit_OverLimitSlotsAreListed(t *testing.T) {
	e := threeCalls(t)
	_ = e
	// Written by the ledger itself with a waiver: the slot is counted over the limit.
	reserveWithWaiver(t, e, "r9", 9950)
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "WARN  OVER LIMIT slot 3") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_ReconcileExposure(t *testing.T) {
	e := newCapsEnv(t)
	// A failed release leaves the slot of a call that never began counted.
	failing := &failRelease{Store: e.as}
	j, err := agent.NewJournal(failing)
	if err != nil {
		t.Fatal(err)
	}
	e.j = j
	_ = e.call("r1", 60, capsRule, capsRuleHr)
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "reconcile exposure, window "+capsDay+": 1 slot(s) worth 60 USD") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(out, "FAIL") {
		t.Fatalf("exposure is information, not a failure:\n%s", out)
	}
}

// failRelease refuses to store a release slot, and the slot of the hour rule: the second rule's
// reservation fails, so the call never begins, and the release of the first rule's slot fails too.
type failRelease struct{ agent.Store }

func (f *failRelease) Insert(ctx context.Context, run, name string, data []byte) (agent.Entry, bool, error) {
	if strings.HasPrefix(name, "slot:") && (strings.Contains(string(data), `"kind":"release"`) || strings.Contains(string(data), "refund_hour")) {
		return agent.Entry{}, false, errors.New("store down")
	}
	return f.Store.Insert(ctx, run, name, data)
}

func (f *failRelease) Unwrap() agent.Store { return f.Store }

// reserveWithWaiver reserves amount in the test rule's ledger as run runID, over the limit under a waiver.
func reserveWithWaiver(t *testing.T, e *capsEnv, runID string, amount int64) {
	t.Helper()
	l := journalhook.CapLedger(e.j, func() time.Time { return capsNow }, 0)
	_, err := l.Reserve(context.Background(), capledger.Request{
		Rule:      capledger.Rule{Namespace: "default", Name: "refund_total", Currency: "USD", Kind: capledger.Day},
		KeyDigest: capledger.KeyDigest(""), RunID: runID, RootRunID: runID, ToolUseID: "c1", AttemptID: "a1",
		Marked: true, Amount: amount, Limit: 10000, Waiver: true,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCapsAudit_RecomputeWindow(t *testing.T) {
	res := func(n int, att string, amt, cum int64) capledger.Slot {
		return capledger.Slot{N: n, Kind: "reserve", Attempt: att, Amount: amt, CumAfter: cum, Currency: "USD"}
	}
	rel := func(n, ref int, att string, amt, cum int64) capledger.Slot {
		return capledger.Slot{N: n, Kind: "release", Attempt: att, Amount: amt, Ref: ref, CumAfter: cum, Currency: "USD"}
	}
	live, total, finds := recomputeWindow([]capledger.Slot{res(0, "a", 30, 30), res(1, "b", 50, 80), rel(2, 0, "a", 30, 50)})
	if live != 1 || total != 50 || len(finds) != 0 {
		t.Fatalf("a released slot is not live: live %d total %d %v", live, total, finds)
	}
	_, _, finds = recomputeWindow([]capledger.Slot{res(0, "a", 30, 30), res(1, "b", 50, 90)})
	if len(finds) != 1 || finds[0].Kind != "TOTAL" || finds[0].Slot != 1 {
		t.Fatalf("a last cum_after that is not the sum of the live reserves must be TOTAL: %v", finds)
	}
	_, _, finds = recomputeWindow([]capledger.Slot{res(0, "a", 30, 30), res(1, "a", 30, 60)})
	if len(finds) != 1 || finds[0].Kind != "DOUBLE" || finds[0].Slot != 1 {
		t.Fatalf("one attempt live twice must be DOUBLE: %v", finds)
	}
	// A re-reserve of an attempt after its release is not a double.
	_, _, finds = recomputeWindow([]capledger.Slot{res(0, "a", 30, 30), rel(1, 0, "a", 30, 0), res(2, "a", 30, 30)})
	if len(finds) != 0 {
		t.Fatalf("an attempt reserved again after its release holds one live slot: %v", finds)
	}
}

func TestCapsAudit_TheReconcileBridgeIsSet(t *testing.T) {
	if capbridge.Voided() == nil {
		t.Fatal("package agent did not register the reconcile bridge: bide audit caps cannot report the reconcile exposure")
	}
}

func TestCapsAudit_AHeadOfAnotherKeyIsRefused(t *testing.T) {
	e := threeCalls(t)
	other, _, _ := ed25519.GenerateKey(nil)
	code, out, errOut := runCLI("caps", "-db", "x.db", "-sth", e.heads(), "-pubkey", hex.EncodeToString(other))
	wantFail(t, code, out, errOut, "BAD_HEAD")
}

func TestCapsAudit_AnAttemptReservedAgainAfterItsReleaseIsNotUnrecorded(t *testing.T) {
	e := newCapsEnv(t)
	ctx := context.Background()
	l := journalhook.CapLedger(e.j, func() time.Time { return capsNow }, 0)
	rule := capledger.Rule{Namespace: "default", Name: "refund_total", Currency: "USD", Kind: capledger.Day}
	req := capledger.Request{Rule: rule, KeyDigest: capledger.KeyDigest(""), RunID: "r1", RootRunID: "r1", ToolUseID: "c1", AttemptID: "a1", Marked: true, Amount: 40, Limit: 10000}
	if _, err := l.Reserve(ctx, req); err != nil {
		t.Fatal(err)
	}
	var word atomic.Int32
	seal, ok := capledger.NewSeal(l.AttemptKey("r1", "r1", "c1", "a1"), &word, 0, 1)
	if !ok {
		t.Fatal("no seal")
	}
	if released, err := l.Release(ctx, capledger.Target{Rule: rule, KeyDigest: req.KeyDigest, RunID: "r1", RootRunID: "r1", ToolUseID: "c1", AttemptID: "a1"}, seal); err != nil || !released {
		t.Fatalf("release = %v, %v", released, err)
	}
	if _, err := l.Reserve(ctx, req); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "3 slot(s), 1 live, window total 40 USD") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_ARecordThatNamesAnotherAttemptsSlot(t *testing.T) {
	e := threeCalls(t)
	// r2's record is rewritten to name slot 0 with slot 0's total: the slot is r1's.
	e.tamper.edit = func(r, n string, d []byte) []byte {
		if r == "r2" && strings.HasPrefix(n, "cap-reserve:") {
			d = bytes.Replace(d, []byte(`"slot":1`), []byte(`"slot":0`), 1)
			d = bytes.Replace(d, []byte(`"cum_after":80`), []byte(`"cum_after":30`), 1)
		}
		return d
	}
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "CHANGED", "UNRECORDED")
	if !strings.Contains(out, "belongs to another attempt") {
		t.Fatalf("CHANGED must say the slot is another attempt's:\n%s", out)
	}
}

func TestCapsAudit_ARecordThatNamesAReleaseSlot(t *testing.T) {
	e := newCapsEnv(t)
	ctx := context.Background()
	l := journalhook.CapLedger(e.j, func() time.Time { return capsNow }, 0)
	rule := capledger.Rule{Namespace: "default", Name: "refund_total", Currency: "USD", Kind: capledger.Day}
	req := capledger.Request{Rule: rule, KeyDigest: capledger.KeyDigest(""), RunID: "r1", RootRunID: "r1", ToolUseID: "c1", AttemptID: "a1", Marked: true, Amount: 40, Limit: 10000}
	if _, err := l.Reserve(ctx, req); err != nil {
		t.Fatal(err)
	}
	var word atomic.Int32
	seal, _ := capledger.NewSeal(l.AttemptKey("r1", "r1", "c1", "a1"), &word, 0, 1)
	if _, err := l.Release(ctx, capledger.Target{Rule: rule, KeyDigest: req.KeyDigest, RunID: "r1", RootRunID: "r1", ToolUseID: "c1", AttemptID: "a1"}, seal); err != nil {
		t.Fatal(err)
	}
	// slot 1 is the release, with total 0.
	e.tamper.edit = func(r, n string, d []byte) []byte {
		if r == "r1" && strings.HasPrefix(n, "cap-reserve:") {
			d = bytes.Replace(d, []byte(`"slot":0`), []byte(`"slot":1`), 1)
			d = bytes.Replace(d, []byte(`"cum_after":40`), []byte(`"cum_after":0`), 1)
		}
		return d
	}
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "CHANGED")
	if !strings.Contains(out, "slot 1, which is a release") {
		t.Fatalf("CHANGED must say the slot is a release:\n%s", out)
	}
}

func TestCapsAudit_NoHeadForALedger(t *testing.T) {
	e := threeCalls(t)
	run := e.ledgerRun()
	e.heads()
	// A head file with the heads of every run but the ledger's.
	var b bytes.Buffer
	for _, en := range e.anchor.Entries() {
		if en.RunID != run {
			line, _ := json.Marshal(en.STH)
			b.Write(append(line, '\n'))
		}
	}
	p := filepath.Join(e.dir, "others.jsonl")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("caps", "-db", "x.db", "-sth", p, "-pubkey", e.pub)
	wantFail(t, code, out, errOut, "NOT_ATTESTED")
	if !strings.Contains(out, "no signed head covers this ledger") {
		t.Fatalf("a ledger no head names is not attested at all:\n%s", out)
	}
}

func TestCapsAudit_AForgedHeadOfADeletedLedgerIsNotCounted(t *testing.T) {
	e := threeCalls(t)
	run := e.ledgerRun()
	heads := e.heads()
	e.tamper.gone = func(r string) bool { return r == run }
	other, _, _ := ed25519.GenerateKey(nil)
	code, out, errOut := runCLI("caps", "-db", "x.db", "-sth", heads, "-pubkey", hex.EncodeToString(other))
	wantFail(t, code, out, errOut, "BAD_HEAD")
	if strings.Contains(out, "a signed head covers this ledger") {
		t.Fatalf("a head that does not verify is evidence of nothing:\n%s", out)
	}
}

func TestCapsAudit_AFilterThatMatchesNothingIsNotAPass(t *testing.T) {
	threeCalls(t)
	for _, args := range [][]string{{"-window", "2026-10-23"}, {"-rule", "nope"}, {"-namespace", "other"}, {"-run", "nope"}} {
		code, out, errOut := unanchoredCaps(args...)
		if code != 4 || strings.Contains(out, "PASS") || !strings.Contains(errOut, "no cap ledger matches") {
			t.Fatalf("%v: exit %d\n%s\n%s", args, code, out, errOut)
		}
	}
	// The window filter keeps the day it names.
	if code, out, errOut := unanchoredCaps("-window", capsDay); code != 0 || !strings.Contains(out, "PASS (filtered)") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}
