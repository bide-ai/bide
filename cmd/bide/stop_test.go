//go:build !bide_audit_only

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/stopcmd"
	"github.com/bide-ai/bide/store/sqlite"
)

// These tests run bide stop through the real command table (run), against real SQLite journals, and read
// the control run back with the library: what the command wrote is what the engine's fold sees.

type stopEnv struct {
	t      *testing.T
	db     string
	policy string
	keys   map[string]string // approver id to key file
}

// newStopEnv makes an empty SQLite journal and, when approvers are given, a stop policy that enrols them:
// the first is the only setter, all of them are lifters, lifts need quorum of them with one not the setter.
func newStopEnv(t *testing.T, quorum int, approvers ...string) *stopEnv {
	t.Helper()
	dir := t.TempDir()
	e := &stopEnv{t: t, db: filepath.Join(dir, "run.db"), keys: map[string]string{}}
	s, err := sqlite.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if len(approvers) == 0 {
		return e
	}
	doc := agent.StopPolicyDoc{DeploymentID: "prod-91c2", Approvers: map[string]agent.StopApprover{}, Setters: approvers[:1], Lifters: approvers, LiftQuorum: quorum, LiftNeedsOther: quorum > 1}
	for _, id := range approvers {
		pub, priv, _ := ed25519.GenerateKey(nil)
		kf := filepath.Join(dir, id+".key")
		if err := os.WriteFile(kf, []byte(hex.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		e.keys[id] = kf
		doc.Approvers[id] = agent.StopApprover{Alg: audit.AlgEd25519, Key: hex.EncodeToString(pub)}
	}
	b, _ := json.Marshal(doc)
	e.policy = filepath.Join(dir, "policy.json")
	if err := os.WriteFile(e.policy, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

// stop runs bide stop on the journal; as, when set, adds the audited flags.
func (e *stopEnv) stop(as string, args ...string) (int, string, string) {
	e.t.Helper()
	full := append([]string{"stop", "-db", e.db}, args...)
	if as != "" {
		full = append(full, "-stop-policy", e.policy, "-as", as, "-key", e.keys[as])
	}
	return runBide(full...)
}

// state is the effective state a basic or audited control reads from the journal.
func (e *stopEnv) state(audited bool) agent.StopState {
	e.t.Helper()
	s, err := sqlite.Open(e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	defer s.Close()
	j, _ := agent.NewJournal(s)
	opts := []agent.StopOption{agent.WithStopMode(agent.StopBasic)}
	if audited {
		doc := agent.StopPolicyDoc{}
		b, _ := os.ReadFile(e.policy)
		_ = json.Unmarshal(b, &doc)
		pol, err := audit.StopPolicyFromDoc(doc)
		if err != nil {
			e.t.Fatal(err)
		}
		opts = []agent.StopOption{agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(pol), agent.WithStopVerifier(audit.StopVerifierFromAudit)}
	}
	ctl, err := agent.NewStopControl(j, opts...)
	if err != nil {
		e.t.Fatal(err)
	}
	st, err := ctl.State(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func TestStop_IsRegisteredAndDocumented(t *testing.T) {
	c, ok := find("stop")
	if !ok {
		t.Fatal("bide stop is not in the command table")
	}
	for _, want := range []string{"5  half done", "3  the journal", "2  usage error"} {
		if !strings.Contains(c.exits, want) {
			t.Errorf("the exit table lacks %q:\n%s", want, c.exits)
		}
	}
	code, out, _ := runBide("help", "stop")
	if code != 0 || !strings.Contains(out, "There is no default scope") || !strings.Contains(out, "Exit status:") || !strings.Contains(out, "-reaudit") {
		t.Fatalf("help stop: exit %d\n%s", code, out)
	}
	if _, out, _ := runBide("help"); !strings.Contains(out, "stop ") {
		t.Fatalf("the command list lacks stop:\n%s", out)
	}
}

func TestStop_BasicSetListLift(t *testing.T) {
	e := newStopEnv(t, 0)
	code, out, errOut := e.stop("", "-all", "-reason", "model misbehaving")
	if code != 0 || !strings.Contains(out, "in force") || !strings.Contains(out, "not signed") {
		t.Fatalf("set: exit %d\n%s\n%s", code, out, errOut)
	}
	st := e.state(false)
	if len(st.Active()) != 1 || st.Active()[0].Scope.Kind != agent.StopGlobal || st.Active()[0].Reason != "model misbehaving" {
		t.Fatalf("the control run holds %+v", st.Stops)
	}
	if _, ok := st.Covering(agent.StopQuery{Tool: "refund"}); !ok {
		t.Fatal("a global stop must cover every call")
	}
	id := st.Active()[0].ID

	// A second identical stop is not a second stop.
	code, out, _ = e.stop("", "-all", "-reason", "again")
	if code != 0 || !strings.Contains(out, "already stopped") || len(e.state(false).Stops) != 1 {
		t.Fatalf("repeat: exit %d\n%s", code, out)
	}

	code, out, _ = e.stop("", "-list")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "basic") || !strings.Contains(out, "asserted, not authenticated") {
		t.Fatalf("list: exit %d\n%s", code, out)
	}
	code, out, _ = e.stop("", "-list", "-json")
	var js struct {
		Mode  string
		Stops []struct {
			ID     string
			Active bool
			Scope  struct{ Kind string }
		}
	}
	if err := json.Unmarshal([]byte(out), &js); code != 0 || err != nil || js.Mode != "basic" || len(js.Stops) != 1 || !js.Stops[0].Active || js.Stops[0].ID != id || js.Stops[0].Scope.Kind != "global" {
		t.Fatalf("list -json: exit %d %v\n%s", code, err, out)
	}
	code, out, _ = e.stop("", "-show", id[:8])
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "model misbehaving") {
		t.Fatalf("show: exit %d\n%s", code, out)
	}

	// A lift is the dangerous direction: with no terminal and no -yes it refuses, and writes nothing.
	code, _, errOut = e.stop("", "-lift", id, "-reason", "fixed")
	if code != 2 || !strings.Contains(errOut, "needs a terminal") || len(e.state(false).Active()) != 1 {
		t.Fatalf("lift without -yes and without a terminal: exit %d\n%s", code, errOut)
	}
	code, out, _ = e.stop("", "-lift", id[:8], "-reason", "fixed", "-yes")
	if code != 0 || !strings.Contains(out, "lifted") || len(e.state(false).Active()) != 0 {
		t.Fatalf("lift: exit %d\n%s", code, out)
	}
	if code, out, _ := e.stop("", "-lift", id, "-reason", "again", "-yes"); code != 0 || !strings.Contains(out, "already lifted") {
		t.Fatalf("lift of a lifted stop: exit %d\n%s", code, out)
	}
	if code, out, _ := e.stop("", "-list"); code != 0 || !strings.Contains(out, "no stop is active") {
		t.Fatalf("list after the lift: exit %d\n%s", code, out)
	}
}

func TestStop_ScopesAndWarnings(t *testing.T) {
	e := newStopEnv(t, 0)
	code, out, errOut := e.stop("", "-tool", "refund", "-tool", "payout", "-flow", "settle", "-agent", "support", "-reason", "incident", "-block-compensation")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "never covered") {
		t.Fatalf("an agent stop must say that a run with no identity is not covered:\n%s", out)
	}
	st := e.state(false)
	if len(st.Active()) != 4 {
		t.Fatalf("want one stop per name, got %+v", st.Stops)
	}
	for _, s := range st.Active() {
		if s.Compensation != agent.CompensationBlock {
			t.Errorf("%v: -block-compensation was not recorded", s.Scope)
		}
	}
	for _, q := range []agent.StopQuery{{Tool: "refund"}, {Tool: "payout"}, {Flow: "settle"}, {Agent: "support"}} {
		if _, ok := st.Covering(q); !ok {
			t.Errorf("%+v is not covered", q)
		}
	}
	if _, ok := st.Covering(agent.StopQuery{Tool: "lookup"}); ok {
		t.Error("a tool stop must not cover another tool")
	}
}

func TestStop_NoDefaultScopeAndUsageErrors(t *testing.T) {
	e := newStopEnv(t, 0)
	// A bare command lists, exits 2, and never sets a stop.
	code, out, errOut := e.stop("")
	if code != 2 || !strings.Contains(errOut, "no scope given") || !strings.Contains(out, "no stop is active") || len(e.state(false).Stops) != 0 {
		t.Fatalf("bare: exit %d\n%s\n%s", code, out, errOut)
	}
	if code, _, errOut := runBide("stop"); code != 2 || !strings.Contains(errOut, "no journal") {
		t.Fatalf("no journal: exit %d\n%s", code, errOut)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no reason", []string{"-all"}, "-reason is required"},
		{"all with tool", []string{"-all", "-tool", "x", "-reason", "r"}, "does not combine"},
		{"scope and verb", []string{"-all", "-list", "-reason", "r"}, "one thing to do"},
		{"json without list", []string{"-all", "-reason", "r", "-json"}, "-json goes with -list"},
		{"key without policy", []string{"-all", "-reason", "r", "-key", "k"}, "give -stop-policy"},
		{"reason on a list", []string{"-list", "-reason", "r"}, "-reason goes with"},
		{"reaudit in basic", []string{"-reaudit"}, "audited deployment"},
		{"upgrade in basic", []string{"-upgrade", "-reason", "r"}, "audited deployment"},
		{"short id", []string{"-lift", "abc", "-reason", "r"}, "at least 8"},
		{"positional", []string{"-all", "-reason", "r", "extra"}, "unexpected argument"},
		{"newline in reason", []string{"-all", "-reason", "a\nb"}, "control or bidirectional"},
		{"empty tool name", []string{"-tool", "", "-reason", "r"}, "name"},
	} {
		code, _, errOut := e.stop("", tc.args...)
		if code != 2 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: exit %d, want 2 and %q\n%s", tc.name, code, tc.want, errOut)
		}
	}
	if n := len(e.state(false).Stops); n != 0 {
		t.Fatalf("a usage error wrote %d stops", n)
	}
	if code, _, errOut := runBide("stop", "-db", filepath.Join(t.TempDir(), "none.db"), "-list"); code != 3 || !strings.Contains(errOut, "none.db") {
		t.Fatalf("a journal that does not exist: exit %d\n%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "none.db")); err == nil {
		t.Fatal("bide stop created a journal")
	}
	t.Setenv("BIDE_TEST_STOP_DSN", "")
	if code, _, errOut := runBide("stop", "-postgres-env", "BIDE_TEST_STOP_DSN", "-list"); code != 3 || !strings.Contains(errOut, "empty or not set") {
		t.Fatalf("an unset DSN variable: exit %d\n%s", code, errOut)
	}
}

func TestStop_AuditedQuorumTrailAndNoSecretsInOutput(t *testing.T) {
	e := newStopEnv(t, 2, "ops", "risk", "cto")
	code, out, errOut := e.stop("ops", "-tool", "refund", "-reason", "provider incident")
	if code != 0 || !strings.Contains(out, "in force") || strings.Contains(out, "not signed") {
		t.Fatalf("set: exit %d\n%s\n%s", code, out, errOut)
	}
	st := e.state(true)
	if len(st.Active()) != 1 || !st.Active()[0].Signed || st.Active()[0].By != "ops" || st.Active()[0].LiftsNeeded != 2 {
		t.Fatalf("state %+v", st.Stops)
	}
	id := st.Active()[0].ID
	seed, _ := os.ReadFile(e.keys["ops"])
	if strings.Contains(out+errOut, strings.TrimSpace(string(seed))) {
		t.Fatal("the key's seed is in the output")
	}
	// The trail holds the stop (and the root): bide audit stops agrees with the control run.
	if code, out, _ := runBide("audit", "stops", "-db", e.db, "-stop-policy", e.policy, "-unanchored"); code != 0 {
		t.Fatalf("audit stops: exit %d\n%s", code, out)
	}

	// The setter's own lift alone is not a quorum, and one lifter cannot be counted twice.
	code, out, _ = e.stop("ops", "-lift", id, "-reason", "recovered", "-yes")
	if code != 0 || !strings.Contains(out, "1 of 2") || len(e.state(true).Active()) != 1 {
		t.Fatalf("first lift: exit %d\n%s", code, out)
	}
	code, out, _ = e.stop("ops", "-lift", id, "-reason", "recovered again", "-yes")
	if code != 0 || len(e.state(true).Active()) != 1 {
		t.Fatalf("a repeated lift by one lifter must not lift: exit %d\n%s", code, out)
	}
	code, out, _ = e.stop("risk", "-lift", id, "-reason", "confirmed", "-yes")
	if code != 0 || !strings.Contains(out, "lifted") || len(e.state(true).Active()) != 0 {
		t.Fatalf("second lifter: exit %d\n%s", code, out)
	}
	if code, out, _ := runBide("audit", "stops", "-db", e.db, "-stop-policy", e.policy, "-unanchored"); code != 0 {
		t.Fatalf("audit stops after the lifts: exit %d\n%s", code, out)
	}
	code, out, _ = e.stop("ops", "-list")
	if code != 0 || !strings.Contains(out, "audited") || !strings.Contains(out, "no stop is active") {
		t.Fatalf("list: exit %d\n%s", code, out)
	}
}

func TestStop_AuditedRefusesWhatThePolicyDoesNotEnrol(t *testing.T) {
	e := newStopEnv(t, 2, "ops", "risk")
	// risk is a lifter, not a setter.
	code, _, errOut := e.stop("risk", "-all", "-reason", "x")
	if code != 1 || !strings.Contains(errOut, "not an enrolled setter") {
		t.Fatalf("a lifter setting: exit %d\n%s", code, errOut)
	}
	// ops signs with risk's key.
	code, _, errOut = runBide("stop", "-db", e.db, "-all", "-reason", "x", "-stop-policy", e.policy, "-as", "ops", "-key", e.keys["risk"])
	if code != 1 || !strings.Contains(errOut, "not the key the stop policy enrols") {
		t.Fatalf("the wrong key: exit %d\n%s", code, errOut)
	}
	code, _, errOut = runBide("stop", "-db", e.db, "-all", "-reason", "x", "-stop-policy", e.policy, "-as", "nobody", "-key", e.keys["ops"])
	if code != 1 || !strings.Contains(errOut, "not an approver of the stop policy") {
		t.Fatalf("an unknown approver: exit %d\n%s", code, errOut)
	}
	if n := len(e.state(true).Stops); n != 0 {
		t.Fatalf("a refused stop wrote %d stops", n)
	}
	// Missing audited flags are usage errors.
	if code, _, errOut := runBide("stop", "-db", e.db, "-all", "-reason", "x", "-stop-policy", e.policy); code != 2 || !strings.Contains(errOut, "-as") {
		t.Fatalf("no -as/-key: exit %d\n%s", code, errOut)
	}
	// An unreadable or invalid policy or key is exit 3 and writes nothing.
	if code, _, errOut := runBide("stop", "-db", e.db, "-all", "-reason", "x", "-stop-policy", filepath.Join(t.TempDir(), "none.json"), "-as", "ops", "-key", e.keys["ops"]); code != 3 {
		t.Fatalf("a policy that does not exist: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := runBide("stop", "-db", e.db, "-all", "-reason", "x", "-stop-policy", e.policy, "-as", "ops", "-key", filepath.Join(t.TempDir(), "none.key")); code != 3 {
		t.Fatalf("a key that does not exist: exit %d\n%s", code, errOut)
	}
	if runtime.GOOS != "windows" {
		bad := filepath.Join(t.TempDir(), "open.key")
		b, _ := os.ReadFile(e.keys["ops"])
		if err := os.WriteFile(bad, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := runBide("stop", "-db", e.db, "-all", "-reason", "x", "-stop-policy", e.policy, "-as", "ops", "-key", bad); code != 3 {
			t.Fatalf("a key file others can read: exit %d\n%s", code, errOut)
		}
	}
}

func TestStop_BasicProcessOnAnAuditedDeploymentIsToldSo(t *testing.T) {
	e := newStopEnv(t, 1, "ops")
	if code, _, errOut := e.stop("ops", "-all", "-reason", "incident"); code != 0 {
		t.Fatalf("set: exit %d\n%s", code, errOut)
	}
	code, out, _ := e.stop("", "-list")
	if code != 0 || !strings.Contains(out, "UNKNOWABLE") || !strings.Contains(out, "NOT verified") {
		t.Fatalf("a basic list of signed records: exit %d\n%s", code, out)
	}
	// A plain lift does not lift a stop set under a signed record.
	code, out, errOut := e.stop("", "-lift", e.state(false).Stops[0].ID, "-reason", "oops", "-yes")
	if code != 1 || !strings.Contains(errOut, "does not lift it") || len(e.state(true).Active()) != 1 {
		t.Fatalf("plain lift: exit %d\n%s\n%s", code, out, errOut)
	}
}

// failingEvents fails every Append while down is set: the stop trail cannot take a record.
type failingEvents struct {
	audit.EventStore
	down *bool
}

func (f failingEvents) Append(ctx context.Context, run string, seq int, leaf []byte) error {
	if *f.down {
		return errors.New("trail unavailable")
	}
	return f.EventStore.Append(ctx, run, seq, leaf)
}

// failingInsert fails Insert of the control run's lift records while down is set: the trail took the lift
// and the control run did not.
type failingInsert struct {
	agent.Store
	down *bool
}

func (f failingInsert) Insert(ctx context.Context, run, name string, data []byte) (agent.Entry, bool, error) {
	if *f.down && run == agent.StopControlRun && strings.HasPrefix(name, "@stop/lift/") {
		return agent.Entry{}, false, errors.New("store unavailable")
	}
	return f.Store.Insert(ctx, run, name, data)
}

func withFaults(t *testing.T, trailDown, storeDown *bool) {
	t.Helper()
	orig := stopStores
	t.Cleanup(func() { stopStores = orig })
	wrap := func(o stopcmd.Opened, err error) (stopcmd.Opened, error) {
		if err != nil {
			return o, err
		}
		o.Events = failingEvents{o.Events, trailDown}
		o.Store = failingInsert{o.Store, storeDown}
		return o, nil
	}
	stopStores = stopcmd.Stores{
		SQLite: func(p string, ro bool) (stopcmd.Opened, error) { return wrap(orig.SQLite(p, ro)) },
	}
}

// Exit 5: a stop in force whose trail write failed is completed by -reaudit, and a repeated stop is not the
// way (it would be a second stop).
func TestStop_HalfLandedStopExits5AndReauditCompletesIt(t *testing.T) {
	e := newStopEnv(t, 1, "ops")
	trailDown, storeDown := new(bool), new(bool)
	withFaults(t, trailDown, storeDown)
	// The first audited write also writes the root, trail first: bring the trail up for it by seeding
	// the root with a harmless first stop, then fail the trail for the stop under test.
	if code, _, errOut := e.stop("ops", "-tool", "seed", "-reason", "seed"); code != 0 {
		t.Fatalf("seed: exit %d\n%s", code, errOut)
	}
	*trailDown = true
	code, _, errOut := e.stop("ops", "-tool", "refund", "-reason", "incident")
	if code != 5 || !strings.Contains(errOut, "in force") || !strings.Contains(errOut, "-reaudit") {
		t.Fatalf("exit %d, want 5\n%s", code, errOut)
	}
	if _, ok := e.state(true).Covering(agent.StopQuery{Tool: "refund"}); !ok {
		t.Fatal("exit 5 on a stop must mean the stop IS in force")
	}
	if code, out, _ := runBide("audit", "stops", "-db", e.db, "-stop-policy", e.policy, "-unanchored"); code != 1 || !strings.Contains(out, "in the control run, not in the trail") {
		t.Fatalf("audit stops must find the missing trail record: exit %d\n%s", code, out)
	}
	*trailDown = false
	code, out, errOut := runBide("stop", "-db", e.db, "-reaudit", "-stop-policy", e.policy)
	if code != 0 || !strings.Contains(out, "appended 1 record") {
		t.Fatalf("reaudit: exit %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ := runBide("audit", "stops", "-db", e.db, "-stop-policy", e.policy, "-unanchored"); code != 0 {
		t.Fatalf("audit stops after reaudit: exit %d\n%s", code, out)
	}
}

// Exit 5 on a lift: the trail took the signed lift and the control run did not, so it is on the trail and
// not in force; -apply-pending shows it with its signer and makes it effective.
func TestStop_HalfLandedLiftExits5AndApplyPendingCompletesIt(t *testing.T) {
	e := newStopEnv(t, 1, "ops")
	trailDown, storeDown := new(bool), new(bool)
	withFaults(t, trailDown, storeDown)
	if code, _, errOut := e.stop("ops", "-all", "-reason", "incident"); code != 0 {
		t.Fatalf("set: exit %d\n%s", code, errOut)
	}
	id := e.state(true).Active()[0].ID
	*storeDown = true
	code, _, errOut := e.stop("ops", "-lift", id, "-reason", "recovered", "-yes")
	if code != 5 || !strings.Contains(errOut, "-apply-pending") {
		t.Fatalf("exit %d, want 5\n%s", code, errOut)
	}
	if len(e.state(true).Active()) != 1 {
		t.Fatal("exit 5 on a lift must mean it is not in force")
	}
	*storeDown = false
	code, out, _ := e.stop("ops", "-list")
	if code != 0 || !strings.Contains(out, "never took effect") {
		t.Fatalf("list must show the pending record: exit %d\n%s", code, out)
	}
	// Without a terminal and without -yes, applying refuses.
	if code, _, errOut := e.stop("ops", "-apply-pending", "-reason", "complete"); code != 2 {
		t.Fatalf("apply-pending without -yes: exit %d\n%s", code, errOut)
	}
	code, out, errOut = e.stop("ops", "-apply-pending", "-reason", "complete the lift", "-yes")
	if code != 0 || !strings.Contains(out, "signed by ops") || !strings.Contains(out, "applied 1") {
		t.Fatalf("apply-pending: exit %d\n%s\n%s", code, out, errOut)
	}
	if len(e.state(true).Active()) != 0 {
		t.Fatal("the pending lift was not applied")
	}
	if code, out, _ := runBide("audit", "stops", "-db", e.db, "-stop-policy", e.policy, "-unanchored"); code != 0 {
		t.Fatalf("audit stops: exit %d\n%s", code, out)
	}
	if code, out, _ := e.stop("ops", "-apply-pending", "-reason", "again", "-yes"); code != 0 || !strings.Contains(out, "nothing is pending") {
		t.Fatalf("a second apply: exit %d\n%s", code, out)
	}
}

func TestStop_UpgradeAdoptsActiveStopsAndDowngradeRefusesWhileActive(t *testing.T) {
	e := newStopEnv(t, 1, "ops")
	if code, _, errOut := e.stop("", "-tool", "refund", "-reason", "plain stop"); code != 0 {
		t.Fatalf("plain set: exit %d\n%s", code, errOut)
	}
	code, out, errOut := e.stop("ops", "-upgrade", "-reason", "go audited", "-yes")
	if code != 0 || !strings.Contains(out, "adopt") {
		t.Fatalf("upgrade: exit %d\n%s\n%s", code, out, errOut)
	}
	st := e.state(true)
	if len(st.Active()) != 1 || !st.Active()[0].Signed || !strings.HasPrefix(st.Active()[0].Reason, "adopted:") || st.Active()[0].AdoptedFrom == "" {
		t.Fatalf("the plain stop was not adopted as a signed one: %+v", st.Stops)
	}
	code, _, errOut = e.stop("ops", "-downgrade", "-reason", "back", "-yes")
	if code != 1 || !strings.Contains(errOut, "lift them before leaving audited mode") {
		t.Fatalf("downgrade with an active stop: exit %d\n%s", code, errOut)
	}
	if code, _, errOut := e.stop("ops", "-lift", st.Active()[0].ID, "-reason", "done", "-yes"); code != 0 {
		t.Fatalf("lift: exit %d\n%s", code, errOut)
	}
	if code, out, errOut := e.stop("ops", "-downgrade", "-reason", "back", "-yes"); code != 0 {
		t.Fatalf("downgrade: exit %d\n%s\n%s", code, out, errOut)
	}
}
