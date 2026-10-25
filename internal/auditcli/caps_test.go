package auditcli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"regexp"
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
	// rename shows an entry under another name (Load only: a planted record in another run).
	rename func(run, name string) string
	edit   func(run, name string, data []byte) []byte
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
			if err == nil && s.rename != nil {
				e.Name = s.rename(run, e.Name)
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
	if code != 0 || !strings.Contains(out, "is unchanged since its newest signed head, and so are the cap records of its runs (a signature covers what the store was given at signing, not that it was legitimate)") || strings.Contains(out, "UNANCHORED") {
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
type failRelease struct {
	agent.Store
	off bool // stop failing
}

func (f *failRelease) Insert(ctx context.Context, run, name string, data []byte) (agent.Entry, bool, error) {
	if !f.off && strings.HasPrefix(name, "slot:") && (strings.Contains(string(data), `"kind":"release"`) || strings.Contains(string(data), "refund_hour")) {
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

func TestCapsAudit_NoReconcileBridgeIsAnError(t *testing.T) {
	e := threeCalls(t)
	led := journalhook.CapLedger(e.j, time.Now, 0)
	if _, err := auditCaps(context.Background(), e.mem, e.j, led, nil, capsInput{}); err == nil {
		t.Fatal("an audit with no reconcile bridge must not report an exposure of zero")
	}
}

// editIntent rewrites the cap-intent of run r.
func (e *capsEnv) editIntent(r string, f func([]byte) []byte) {
	e.tamper.edit = func(run, n string, d []byte) []byte {
		if run == r && strings.HasPrefix(n, "cap-intent:") {
			return f(d)
		}
		return d
	}
}

func replaceIn(old, new string) func([]byte) []byte {
	return func(d []byte) []byte { return bytes.Replace(d, []byte(old), []byte(new), 1) }
}

func TestCapsAudit_IntentMissing(t *testing.T) {
	e := threeCalls(t)
	e.tamper.hide = func(r, n string) bool { return r == "r2" && strings.HasPrefix(n, "cap-intent:") }
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_MISSING")
	if !strings.Contains(out, "slot 1 (attempt r2/") {
		t.Fatalf("INTENT_MISSING must name slot 1:\n%s", out)
	}
}

func TestCapsAudit_IntentWithAnotherWindow(t *testing.T) {
	e := threeCalls(t)
	// The window-pinning attack: the intent fixes another window than the one the slot was counted in.
	e.editIntent("r2", replaceIn(`"window":"2026-10-22"`, `"window":"2026-10-23"`))
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_MISMATCH", "INDEX_MISSING")
	if !strings.Contains(out, "the intent fixes window 2026-10-23, the slot is in window 2026-10-22") {
		t.Fatalf("the window must be named:\n%s", out)
	}
}

func TestCapsAudit_IntentThatDiffersFromItsSlot(t *testing.T) {
	for name, edit := range map[string]struct {
		f    func([]byte) []byte
		want string
	}{
		"amount":   {replaceIn(`"amount":50`, `"amount":51`), "the intent reserves 51, the slot holds 50"},
		"marked":   {replaceIn(`"marked":true,`, ``), "the intent's marking is false, the slot's is true"},
		"currency": {replaceIn(`"currency":"USD"`, `"currency":"EUR"`), "the intent's currency is EUR, the slot's is USD"},
		"rule":     {replaceIn(`"rule":"refund_total"`, `"rule":"refund_other"`), "the intent names rule refund_other"},
		"key":      {replaceIn(`"key":"cd72`, `"key":"ab72`), "the intent names rule refund_total, key ab72"},
	} {
		e := threeCalls(t)
		e.editIntent("r2", edit.f)
		code, out, errOut := unanchoredCaps()
		if code != 1 || !strings.Contains(out, "FAIL  INTENT_MISMATCH") || !strings.Contains(out, edit.want) {
			t.Fatalf("%s: exit %d, want %q\n%s\n%s", name, code, edit.want, out, errOut)
		}
	}
}

func TestCapsAudit_IntentPlantedInAnotherRun(t *testing.T) {
	e := threeCalls(t)
	// r3's intent is renamed to r2's attempt: r2's attempt has two intents, and r3's slot has none.
	r2 := "r2%2Fr2%2Fc1%2Fattempt%253Atool%253Ac1%2523refund_total"
	r3 := "r3%2Fr3%2Fc1%2Fattempt%253Atool%253Ac1%2523refund_total"
	e.tamper.edit = func(run, n string, d []byte) []byte {
		if run == "r3" && n == "cap-intent:"+r3 {
			d = bytes.ReplaceAll(d, []byte(r3), []byte(r2))
			d = bytes.Replace(d, []byte(`"attempt":"r3/r3/`), []byte(`"attempt":"r2/r2/`), 1)
		}
		return d
	}
	e.tamper.rename = func(run, n string) string {
		if run == "r3" && n == "cap-intent:"+r3 {
			return "cap-intent:" + r2
		}
		return n
	}
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_DUPLICATE", "INTENT_MISSING")
}

func TestCapsAudit_IntentUnreadable(t *testing.T) {
	e := threeCalls(t)
	e.editIntent("r1", replaceIn(`"amount":30`, `"amount":30,"extra":1`))
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_UNREADABLE", "INTENT_MISSING")
	if !strings.Contains(out, "unknown field") {
		t.Fatalf("the decode error must be shown:\n%s", out)
	}
}

func TestCapsAudit_IntentUnderAnotherAttemptsName(t *testing.T) {
	e := threeCalls(t)
	e.editIntent("r1", replaceIn(`"attempt":"r1/r1/`, `"attempt":"r1/r9/`))
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_UNREADABLE")
}

func TestCapsAudit_IntentWithNoSlotIsInformation(t *testing.T) {
	e := threeCalls(t)
	// 99 USD more would take the day over 100 USD: refused, so an intent and no slot.
	_ = e.call("r4", 9990)
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "INFO  1 cap-intent(s) have no slot") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_IndexEntryDeleted(t *testing.T) {
	e := threeCalls(t)
	e.tamper.hide = func(r, n string) bool { return strings.HasSuffix(r, "/index") && n == "win:"+capsDay }
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INDEX_MISSING")
	if !strings.Contains(out, "FAIL  INDEX_MISSING @cap/default/refund_total/USD/index") {
		t.Fatalf("the index run must be named:\n%s", out)
	}
}

func TestCapsAudit_IntentWindowEarlierThanItsFloor(t *testing.T) {
	e := threeCalls(t)
	e.editIntent("r2", replaceIn(`"floor":"2026-10-22"`, `"floor":"2026-10-23"`))
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_CLOCK")
}

func TestCapsAudit_IntentInAnotherRunThanTheSlotsCall(t *testing.T) {
	e := threeCalls(t)
	run := e.ledgerRun()
	// slot 1 claims to be a call of r3; the intent of its attempt is in r2.
	e.tamper.edit = func(r, n string, d []byte) []byte {
		if r == run && n == "slot:1" {
			d = bytes.Replace(d, []byte(`"run":"r2"`), []byte(`"run":"r3"`), 1)
		}
		return d
	}
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_MISMATCH")
	if !strings.Contains(out, "the intent is in run r2, the slot's call is in run r3") {
		t.Fatalf("the runs must be named:\n%s", out)
	}
}

func TestCapsAudit_TwoIntentsAndNoSlot(t *testing.T) {
	e := threeCalls(t)
	_ = e.call("r4", 9990)
	_ = e.call("r5", 9990)
	r4 := "r4%2Fr4%2Fc1%2Fattempt%253Atool%253Ac1%2523refund_total"
	r5 := "r5%2Fr5%2Fc1%2Fattempt%253Atool%253Ac1%2523refund_total"
	e.tamper.edit = func(run, n string, d []byte) []byte {
		if run == "r5" && n == "cap-intent:"+r5 {
			d = bytes.ReplaceAll(d, []byte(r5), []byte(r4))
			d = bytes.Replace(d, []byte(`"attempt":"r5/r5/`), []byte(`"attempt":"r4/r4/`), 1)
		}
		return d
	}
	e.tamper.rename = func(run, n string) string {
		if run == "r5" && n == "cap-intent:"+r5 {
			return "cap-intent:" + r4
		}
		return n
	}
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_DUPLICATE")
	if !strings.Contains(out, "and no slot") {
		t.Fatalf("the duplicate of a slotless attempt must say so:\n%s", out)
	}
}

func TestCapsAudit_APerRunRuleNeedsNoIndex(t *testing.T) {
	e := newCapsEnv(t)
	if err := e.call("r1", 30, `run_total: refund.amount[minor, currency=currency] sum 100 USD per run -> refuse`); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "1 slot(s)") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_IntentOfALedgerOutsideTheFilter(t *testing.T) {
	e := threeCalls(t)
	_ = e.call("r4", 7, `refund_other: refund.amount[minor, currency=currency] sum 100 USD per day -> refuse`)
	e.tamper.hide = func(r, n string) bool { return r == "r4" && strings.HasPrefix(n, "cap-intent:") }
	// The damage is in refund_other's ledger: an audit of refund_total does not look, and says so.
	code, out, errOut := unanchoredCaps("-rule", "refund_total")
	if code != 0 || !strings.Contains(out, "NOT CHECKED") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = unanchoredCaps("-rule", "refund_other")
	wantFail(t, code, out, errOut, "INTENT_MISSING")
}

func TestCapsAudit_AFilterDoesNotCallAnotherLedgersIntentsOrphans(t *testing.T) {
	e := threeCalls(t)
	_ = e.call("r4", 7, `refund_other: refund.amount[minor, currency=currency] sum 100 USD per day -> refuse`)
	code, out, errOut := unanchoredCaps("-rule", "refund_total")
	if code != 0 || strings.Contains(out, "have no slot") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// forgeRelease appends a valid release of the reservation of run's call c1 under rule, through the
// store the runs were written to (an AuditedStore, so the head covers it): a release the loop never wrote.
func (e *capsEnv) forgeRelease(run string) {
	e.t.Helper()
	l := journalhook.CapLedger(e.j, func() time.Time { return capsNow }, 0)
	rule := capledger.Rule{Namespace: "default", Name: "refund_total", Currency: "USD", Kind: capledger.Day}
	att := "attempt:tool:c1#refund_total"
	var word atomic.Int32
	seal, ok := capledger.NewSeal(l.AttemptKey(run, run, "c1", att), &word, 0, 1)
	if !ok {
		e.t.Fatal("no seal")
	}
	released, err := l.Release(context.Background(), capledger.Target{Rule: rule, KeyDigest: capledger.KeyDigest(""), RunID: run, RootRunID: run, ToolUseID: "c1", AttemptID: att}, seal)
	if err != nil || !released {
		e.t.Fatalf("release = %v, %v", released, err)
	}
}

func TestCapsAudit_AReleaseOfACallThatRan(t *testing.T) {
	e := threeCalls(t)
	e.forgeRelease("r1") // r1's refund ran and answered "ok"
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "RELEASE_OF_A_CALL_THAT_RAN")
	if !strings.Contains(out, "slot 3 releases slot 0") {
		t.Fatalf("the release and its reserve must be named:\n%s", out)
	}
	// Written through the signing store, the head covers the forgery, and the audit still finds it.
	code, out, errOut = e.anchored()
	wantFail(t, code, out, errOut, "RELEASE_OF_A_CALL_THAT_RAN")
}

func TestCapsAudit_ALegitimateReleaseIsNotFlagged(t *testing.T) {
	e := threeCalls(t)
	_ = e.call("r4", 150, capsRule, capsRuleHr) // refused by the second rule: the first slot is released
	for name, run := range map[string]func() (int, string, string){"unanchored": func() (int, string, string) { return unanchoredCaps() }, "anchored": func() (int, string, string) { return e.anchored() }} {
		code, out, errOut := run()
		if code != 0 || strings.Contains(out, "RELEASE_") {
			t.Fatalf("%s: exit %d\n%s\n%s", name, code, out, errOut)
		}
	}
}

func TestCapsAudit_AReleaseWithNeitherAVoidNorAResultIsAWarning(t *testing.T) {
	e := threeCalls(t)
	e.forgeRelease("r1")
	e.tamper.hide = func(r, n string) bool { return r == "r1" && n == "tool:c1" }
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "WARN  RELEASE_UNCONFIRMED") || strings.Contains(out, "FAIL") {
		t.Fatalf("a crash between the release and the void looks the same: exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_AReleaseOfAVoidedAttemptIsClean(t *testing.T) {
	e := newCapsEnv(t)
	failing := &failRelease{Store: e.as}
	j, err := agent.NewJournal(failing)
	if err != nil {
		t.Fatal(err)
	}
	e.j = j
	_ = e.call("r1", 60, capsRule, capsRuleHr) // voided, its release failed
	ctx := context.Background()
	failing.off = true
	l := journalhook.CapLedger(j, func() time.Time { return capsNow }, 0)
	released, err := l.Reconcile(ctx, l.LedgerRun(capledger.Rule{Namespace: "default", Name: "refund_total", Currency: "USD", Kind: capledger.Day}, capsDay, capledger.KeyDigest("")), func(ctx context.Context, s capledger.Slot) (bool, error) { return capbridge.Voided()(ctx, j, s) })
	if err != nil || len(released) != 1 {
		t.Fatalf("reconcile = %v, %v", released, err)
	}
	code, out, errOut := unanchoredCaps()
	if code != 0 || strings.Contains(out, "RELEASE_") || strings.Contains(out, "reconcile exposure") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_UnrecordedSlotOfARunThatHasNotEnded(t *testing.T) {
	e := threeCalls(t)
	e.tamper.hide = func(r, n string) bool {
		return r == "r2" && (strings.HasPrefix(n, "cap-reserve:") || n == "run:complete")
	}
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "WARN  UNRECORDED") || strings.Contains(out, "FAIL") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	// The same record gone from a run that ended is a failure (TestCapsAudit_CapReserveDeleted).
}

func TestCapsAudit_HeadFiles(t *testing.T) {
	e := threeCalls(t)
	heads := e.heads()
	write := func(name, content string) string {
		p := filepath.Join(e.dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, content := range map[string]string{"malformed": "{not json", "empty": "", "unknown member": `{"format":"x","bogus":1}`} {
		if code, out, errOut := runCLI("caps", "-db", "x.db", "-sth", write(name, content), "-pubkey", e.pub); code != 4 || strings.Contains(out, "PASS") {
			t.Fatalf("%s: exit %d, want 4\n%s\n%s", name, code, out, errOut)
		}
	}
	// The same heads twice, or in two files, are the same evidence.
	if code, out, errOut := runCLI("caps", "-db", "x.db", "-sth", heads, "-sth", heads, "-pubkey", e.pub); code != 0 {
		t.Fatalf("duplicate head files: exit %d\n%s\n%s", code, out, errOut)
	}
	// A missing head file is an unusable input, not a pass.
	if code, _, _ := runCLI("caps", "-db", "x.db", "-sth", filepath.Join(e.dir, "nope"), "-pubkey", e.pub); code != 4 {
		t.Fatalf("missing head file: exit %d, want 4", code)
	}
}

func TestCapsAudit_JSONSchemaAndStatesMatchTheTextAndTheExit(t *testing.T) {
	e := threeCalls(t)
	e.forgeRelease("r1")
	for name, c := range map[string]struct {
		hide func(r, n string) bool
		want int
	}{
		"fail": {nil, 1},
		"warn": {func(r, n string) bool { return r == "r1" && n == "tool:c1" }, 0},
	} {
		e.tamper.hide = c.hide
		codeJ, outJ, _ := unanchoredCaps("-json")
		codeT, outT, _ := unanchoredCaps()
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(outJ), &raw); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, k := range []string{"tool", "version", "verb", "exit_code", "result", "verified", "output", "errors", "artifact"} {
			if _, ok := raw[k]; !ok {
				t.Fatalf("%s: the -json report has no %q member", name, k)
			}
		}
		var rep struct {
			ExitCode int      `json:"exit_code"`
			Verified bool     `json:"verified"`
			Result   string   `json:"result"`
			Output   []string `json:"output"`
			Artifact struct {
				Findings []struct {
					Kind string `json:"kind"`
					Warn bool   `json:"warn"`
				} `json:"findings"`
			} `json:"artifact"`
		}
		_ = json.Unmarshal([]byte(outJ), &rep)
		if codeJ != c.want || codeT != c.want || rep.ExitCode != c.want || rep.Verified != (c.want == 0) || (c.want == 0) != (rep.Result == "verified") {
			t.Fatalf("%s: exits json %d text %d, report %+v", name, codeJ, codeT, rep)
		}
		if strings.Join(rep.Output, "\n") != strings.TrimRight(outT, "\n") {
			t.Fatalf("%s: the -json output lines differ from the text report:\n%v\n%s", name, rep.Output, outT)
		}
		if len(rep.Artifact.Findings) != 1 || rep.Artifact.Findings[0].Warn != (c.want == 0) {
			t.Fatalf("%s: findings %+v", name, rep.Artifact.Findings)
		}
	}
}

func TestCapsAudit_AForgedReleaseOfAnotherRunIsOutsideRun(t *testing.T) {
	e := threeCalls(t)
	e.forgeRelease("r1")
	code, out, errOut := unanchoredCaps("-run", "r2")
	if code != 0 || strings.Contains(out, "RELEASE_") || !strings.Contains(out, "NOT CHECKED") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = unanchoredCaps("-run", "r1")
	wantFail(t, code, out, errOut, "RELEASE_OF_A_CALL_THAT_RAN")
}

// plantIntent writes, with the raw store (not the signing one), a cap-intent into run's journal for the
// refund call c1 of run, fixing window, and the matching win: entry in the rule's index run: the
// window-pinning attack, for amount minor units.
func (e *capsEnv) plantIntent(run, window string, amount int64) {
	e.t.Helper()
	ctx := context.Background()
	salt := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	insert := func(r, name string, result any) {
		body := map[string]any{"name": name, "kind": "value", "salt": salt}
		if result != nil {
			body["result"] = result
		}
		if name == "@journal" {
			body["kind"], body["format"] = "header", "bide.journal.v1-dev"
		}
		b, _ := json.Marshal(body)
		if _, _, err := e.mem.Insert(ctx, r, name, b); err != nil {
			e.t.Fatal(err)
		}
	}
	insert(run, "@journal", nil)
	attempt := run + "/" + run + "/c1/attempt%3Atool%3Ac1%23refund_total"
	name := "cap-intent:" + strings.NewReplacer("/", "%2F", "%", "%25").Replace(attempt)
	insert(run, name, map[string]any{"format": "bide.cap-intent.v1", "attempt": attempt, "namespace": "default", "rule": "refund_total",
		"currency": "USD", "window_kind": "day", "key": capledger.KeyDigest(""), "amount": amount, "marked": true, "window": window})
	insert("@cap/default/refund_total/USD/index", "win:"+window, map[string]any{"window": window})
}

func TestCapsAudit_AWindowPinnedByAPlantedIntent(t *testing.T) {
	e := newCapsEnv(t)
	if err := e.call("r0", 8000); err != nil { // 80 of the day's 100 USD
		t.Fatal(err)
	}
	e.plantIntent("r1", "2026-10-01", 9000)
	if err := e.call("r1", 9000); err != nil {
		t.Fatal(err)
	}
	// The engine adopted the intent: the 90 USD landed in the old day's ledger, and every ledger reads fine.
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_WINDOW")
	if !strings.Contains(out, "a call cannot be counted in an earlier window") || strings.Count(out, "INFO  ledger ") != 2 {
		t.Fatalf("both ledgers are read and the pinned window is named:\n%s", out)
	}
	// -window does not hide it: the pinned ledger is outside the filter, and the report says so.
	code, out, errOut = unanchoredCaps("-window", capsDay)
	wantFail(t, code, out, errOut, "INTENT_WINDOW")
	if !strings.Contains(out, "fix a window the filter left out") {
		t.Fatalf("the ledger the filter left out must be listed:\n%s", out)
	}
	// Anchored too.
	code, out, errOut = e.anchored()
	wantFail(t, code, out, errOut, "INTENT_WINDOW")
}

func TestWindowOutOfTime(t *testing.T) {
	day := func(s string) time.Time { tm, _ := time.Parse(time.RFC3339, s); return tm }
	for _, c := range []struct {
		name      string
		kind      capledger.Kind
		window    string
		start     time.Time
		haveStart bool
		marked    time.Time
		bad       bool
	}{
		{"same day", capledger.Day, "2026-10-22", day("2026-10-22T10:00:00Z"), true, day("2026-10-22T10:00:01Z"), false},
		{"a call just before midnight reserving into the next day", capledger.Day, "2026-10-23", day("2026-10-22T23:59:59Z"), true, day("2026-10-22T23:59:59Z"), false},
		{"a window one day earlier", capledger.Day, "2026-10-21", day("2026-10-22T00:00:01Z"), true, day("2026-10-22T00:00:01Z"), true},
		{"an old window", capledger.Day, "2026-10-01", day("2026-10-22T10:00:00Z"), true, day("2026-10-22T10:00:00Z"), true},
		{"a long run started days earlier, pinned to a day between", capledger.Day, "2026-10-10", day("2026-10-01T10:00:00Z"), true, day("2026-10-22T10:00:00Z"), true},
		{"two days ahead of the marker", capledger.Day, "2026-10-24", day("2026-10-22T10:00:00Z"), true, day("2026-10-22T10:00:00Z"), true},
		{"the hour after", capledger.Hour, "2026-10-22T11", day("2026-10-22T10:59:59Z"), true, day("2026-10-22T10:59:59Z"), false},
		{"two hours ahead of the marker", capledger.Hour, "2026-10-22T12", day("2026-10-22T10:00:00Z"), true, day("2026-10-22T10:00:00Z"), true},
		{"the hour before", capledger.Hour, "2026-10-22T09", day("2026-10-22T10:00:00Z"), true, day("2026-10-22T10:00:00Z"), true},
		{"unmarked: a window earlier than the run's start", capledger.Day, "2026-10-01", day("2026-10-22T10:00:00Z"), true, time.Time{}, true},
		{"unmarked: any window from the start on (the limit)", capledger.Day, "2026-11-01", day("2026-10-22T10:00:00Z"), true, time.Time{}, false},
	} {
		if got := windowOutOfTime(c.kind, c.window, c.start, c.haveStart, c.marked); (got != "") != c.bad {
			t.Errorf("%s: windowOutOfTime = %q, want bad=%v", c.name, got, c.bad)
		}
	}
}

func TestCapsAudit_ARunWindowThatIsNotTheCallsRoot(t *testing.T) {
	e := newCapsEnv(t)
	if err := e.call("r1", 30, `run_total: refund.amount[minor, currency=currency] sum 100 USD per run -> refuse`); err != nil {
		t.Fatal(err)
	}
	e.editIntent("r1", replaceIn(`"window":"r1"`, `"window":"r9"`))
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_WINDOW")
}

func TestCapsAudit_AnIntentThatNothingDates(t *testing.T) {
	e := threeCalls(t)
	e.tamper.hide = func(r, n string) bool { return r == "r2" && (n == "run:start" || strings.HasPrefix(n, "attempt:")) }
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "WARN  INTENT_UNDATED") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_RunsWhoseCapRecordsNoHeadCovers(t *testing.T) {
	e := threeCalls(t)
	e.heads()
	write := func(keep func(en audit.AnchorEntry) bool) string {
		var b bytes.Buffer
		for _, en := range e.anchor.Entries() {
			if keep(en) {
				line, _ := json.Marshal(en.STH)
				b.Write(append(line, '\n'))
			}
		}
		p := filepath.Join(e.dir, "partial.jsonl")
		if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Only the ledger's heads: no run is covered.
	p := write(func(en audit.AnchorEntry) bool { return strings.HasPrefix(en.RunID, "@cap/") })
	code, out, errOut := runCLI("caps", "-db", "x.db", "-sth", p, "-pubkey", e.pub)
	wantFail(t, code, out, errOut, "RUN_NOT_ATTESTED")
	if !strings.Contains(out, "no signed head covers run r1") {
		t.Fatalf("each run must be named:\n%s", out)
	}
	// r2's oldest head only: its cap records run past it.
	seen := false
	p = write(func(en audit.AnchorEntry) bool {
		if en.RunID != "r2" {
			return true
		}
		first := !seen
		seen = true
		return first
	})
	code, out, errOut = runCLI("caps", "-db", "x.db", "-sth", p, "-pubkey", e.pub)
	wantFail(t, code, out, errOut, "RUN_NOT_ATTESTED")
	if !strings.Contains(out, "run r2: its cap records run to entry") {
		t.Fatalf("r2's head stops short:\n%s", out)
	}
	// A cap-intent changed after its run's head was signed.
	e.editIntent("r2", replaceIn(`"amount":50`, `"amount":51`))
	code, out, errOut = e.anchored()
	wantFail(t, code, out, errOut, "DIVERGENT_HEAD")
}

func TestCapsAudit_AWindowPinnedIntoTheFuture(t *testing.T) {
	e := newCapsEnv(t)
	e.plantIntent("r1", "2026-12-01", 100)
	if err := e.call("r1", 100); err != nil {
		t.Fatal(err)
	}
	// The attempt marker was claimed now, and the intent fixes a window months ahead.
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_WINDOW")
	if !strings.Contains(out, "a call cannot be counted in a window past the next one") {
		t.Fatalf("the future window must be named:\n%s", out)
	}
}

func TestCapsAudit_ALongRunPinsToAnEarlierDay(t *testing.T) {
	e := newCapsEnv(t)
	e.plantIntent("r1", "2026-10-10", 100)
	if err := e.call("r1", 100); err != nil {
		t.Fatal(err)
	}
	// The run claims to have started on Oct 1: only the attempt marker (stamped by the engine from the
	// run's clock, Oct 22) dates the call, and the window between is not allowed.
	e.tamper.edit = func(r, n string, d []byte) []byte {
		if r == "r1" && n == "run:start" {
			d = bytes.Replace(d, []byte(`"started_at":"2026-10-22T10:00:00.000Z"`), []byte(`"started_at":"2026-10-01T10:00:00.000Z"`), 1)
		}
		return d
	}
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_WINDOW")
	if !strings.Contains(out, "the attempt was claimed in window 2026-10-22") {
		t.Fatalf("the marker must date the call:\n%s", out)
	}
}

func TestCapsAudit_TheEngineRecordsTheMarkerOnTheRunsClock(t *testing.T) {
	e := threeCalls(t)
	recs, err := e.j.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Kind == agent.StepAttempt {
			got, err := agent.ParseTimestamp(r.RecordedAt())
			if err != nil || !got.Equal(capsNow) {
				t.Fatalf("the marker's recorded_at is %q (%v), want the run's clock %v", r.RecordedAt(), err, capsNow)
			}
			return
		}
	}
	t.Fatal("no attempt marker")
}

func TestCapsAudit_AnUnmarkedAttemptIsDatedByItsRunsStartAndSaysSo(t *testing.T) {
	e := newCapsEnv(t)
	type in struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	tool, err := agent.Func("refund", "refund", func(context.Context, in) (string, error) { return "ok", nil },
		agent.WithSafety(agent.Safety{Idempotent: true}), agent.WithCaps(capsRule))
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{"amount":30,"currency":"USD"}`), agenttest.TextTurn("done")),
		e.j, agent.WithTools(tool), agent.WithClock(func() time.Time { return capsNow }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r1", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "1 cap-intent(s) belong to attempts with no dated marker") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// TestCapsAudit_APausedRunPinnedBackToItsStartDay: the reviewer's probe. A run waits at an approval for two
// days; an intent planted while it waited fixes the day it started, and the resumed drive adopts it. The
// attempt marker is claimed when the run resumes (the engine stamps it from the run's clock), so the audit
// sees a window two days before the claim.
func TestCapsAudit_APausedRunPinnedBackToItsStartDay(t *testing.T) {
	e := newCapsEnv(t)
	if err := e.call("r0", 8000); err != nil { // 80 of the day's 100 USD, on Oct 22
		t.Fatal(err)
	}
	type in struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	tool, err := agent.Func("refund", "refund", func(context.Context, in) (string, error) { return "ok", nil },
		agent.WithCaps(capsRule), agent.WithApproval(agent.SingleApproval()))
	if err != nil {
		t.Fatal(err)
	}
	drive := func(now time.Time) error {
		a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{"amount":9000,"currency":"USD"}`), agenttest.TextTurn("done")),
			e.j, agent.WithTools(tool), agent.WithClock(func() time.Time { return now }))
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.Run(context.Background(), "r1", agent.UserText("go"))
		return err
	}
	if err := drive(capsNow); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatalf("drive 1 = %v, want the approval gate", err)
	}
	e.plantIntent("r1", capsDay, 9000) // while it waits: the day it started, with room for 90 only if r0 is not counted
	if err := agent.Approve(context.Background(), e.j, "r1", "c1", true); err != nil {
		t.Fatal(err)
	}
	if err := drive(capsNow.Add(48 * time.Hour)); err != nil {
		t.Fatalf("drive 2 = %v", err)
	}
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_WINDOW")
	if !strings.Contains(out, "the attempt was claimed in window 2026-10-24: a call cannot be counted in an earlier window") {
		t.Fatalf("the claim's window must be named:\n%s", out)
	}
	code, out, errOut = e.anchored()
	wantFail(t, code, out, errOut, "INTENT_WINDOW")
}

func TestCapsAudit_AMarkerWithoutRecordedAtDatesNothingAndSaysSo(t *testing.T) {
	e := newCapsEnv(t)
	e.plantIntent("r1", "2026-10-10", 100) // pinned to an earlier day
	if err := e.call("r1", 100); err != nil {
		t.Fatal(err)
	}
	// A marker written before recorded_at existed: with it gone, only run:start (the same day) dates the
	// call, so the pin to an earlier day is still caught, and the note says the marker dated nothing.
	e.tamper.edit = func(r, n string, d []byte) []byte {
		if r == "r1" && strings.HasPrefix(n, "attempt:") {
			d = regexp.MustCompile(`,?"recorded_at":"[^"]*"`).ReplaceAll(d, nil)
		}
		return d
	}
	code, out, errOut := unanchoredCaps()
	wantFail(t, code, out, errOut, "INTENT_WINDOW")
	if !strings.Contains(out, "no dated marker") {
		t.Fatalf("the undated marker must be counted:\n%s", out)
	}
	// And with a plain intent the audit passes but still says the call was dated by run:start only.
	e2 := newCapsEnv(t)
	if err := e2.call("r1", 100); err != nil {
		t.Fatal(err)
	}
	e2.tamper.edit = e.tamper.edit
	code, out, errOut = unanchoredCaps()
	if code != 0 || !strings.Contains(out, "NOT CHECKED: 1 cap-intent(s) belong to attempts with no dated marker") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestCapsAudit_AForgedReleaseOfACallThatRanAndFailedIsAWarning(t *testing.T) {
	e := newCapsEnv(t)
	type in struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	tool, err := agent.Func("refund", "refund", func(context.Context, in) (string, error) { return "", errors.New("boom") }, agent.WithCaps(capsRule))
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{"amount":60,"currency":"USD"}`), agenttest.TextTurn("done")),
		e.j, agent.WithTools(tool), agent.WithClock(func() time.Time { return capsNow }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r1", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	e.forgeRelease("r1") // the tool ran and failed: its marker stands, and it was never released by the loop
	// The tool's own error is not the engine's refusal: the audit cannot tell it from one it does not know,
	// and says so (a warning), where a legitimate refusal passes silently.
	for name, run := range map[string]func() (int, string, string){"unanchored": func() (int, string, string) { return unanchoredCaps() }, "anchored": func() (int, string, string) { return e.anchored() }} {
		code, out, errOut := run()
		if code != 0 || !strings.Contains(out, "WARN  RELEASE_UNCONFIRMED") || !strings.Contains(out, "an error the engine did not write as a refusal") {
			t.Fatalf("%s: exit %d\n%s\n%s", name, code, out, errOut)
		}
	}
}

func TestCapsAudit_AReleaseJudgedOnRecordsPastTheRunsNewestHead(t *testing.T) {
	e := threeCalls(t)
	e.heads()
	// r1's newest head is the one through its last cap record; the rest of the run (result, completion) is past it.
	recs, err := e.j.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for i, r := range recs {
		if capledger.IsIntentName(r.Name) || capledger.IsReserveName(r.Name) {
			size = i + 1
		}
	}
	var b bytes.Buffer
	for _, en := range e.anchor.Entries() {
		if en.RunID == "r1" && en.STH.Size != size {
			continue
		}
		line, _ := json.Marshal(en.STH)
		b.Write(append(line, '\n'))
	}
	p := filepath.Join(e.dir, "short.jsonl")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	e.forgeRelease("r1")
	// What shows whether r1's call ran (its result) lies past the run's newest head: the release is not attested.
	code, out, errOut := runCLI("caps", "-db", "x.db", "-sth", p, "-pubkey", e.pub)
	wantFail(t, code, out, errOut, "RELEASE_UNATTESTED")
}

func TestCapsAudit_APassSaysHowManyWarningsItCarries(t *testing.T) {
	e := threeCalls(t)
	e.forgeRelease("r1")
	e.tamper.hide = func(r, n string) bool { return r == "r1" && n == "tool:c1" }
	code, out, errOut := unanchoredCaps()
	if code != 0 || !strings.Contains(out, "(with 1 warning(s) above)") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}
