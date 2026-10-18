package journalcmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/internal/auditcli"
	"github.com/bide-ai/bide/store/sqlite"
)

type rig struct {
	t                       *testing.T
	dir, db, key, pub, head string
	headOut, trailDB        string
	signer                  audit.Signer
	verifier                audit.Verifier
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, dir: dir, db: filepath.Join(dir, "journal.db"), key: filepath.Join(dir, "log.key"), headOut: filepath.Join(dir, "heads.jsonl")}
	var out, errb bytes.Buffer
	if code := approvecmd.KeygenMain([]string{"-out", r.key, "-color", "never"}, &out, &errb); code != 0 {
		t.Fatalf("keygen: %d %s", code, errb.String())
	}
	r.pub = r.key + ".pub"
	var err error
	if r.signer, err = approvecmd.LoadSigner(r.key); err != nil {
		t.Fatal(err)
	}
	r.verifier = mustVerifier(t, r.pub)
	s, err := sqlite.Open(r.db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j := agenttest.MustJournal(s)
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("your account 4111-1111 is fine")), j)
	if _, err := a.Run(context.Background(), "r1", agent.UserText("my name is Alice, account 4111-1111")); err != nil {
		t.Fatal(err)
	}
	th, err := audit.NewTreeHead(context.Background(), j, "r1", time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	h, err := audit.SignTreeHead(th, r.signer)
	if err != nil {
		t.Fatal(err)
	}
	r.head = filepath.Join(dir, "r1.sth")
	b, _ := json.Marshal(h)
	if err := os.WriteFile(r.head, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func mustVerifier(t *testing.T, path string) audit.Verifier {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.ParsePublicKey(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (r *rig) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(append([]string{"redact", "-color", "never"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func (r *rig) base() []string { return []string{"-db", r.db, "-run", "r1", "-payloads"} }

func (r *rig) execArgs(digest string, extra ...string) []string {
	a := append(r.base(), "-execute", "-plan-digest", digest, "-reason", "customer request", "-ticket", "PRIV-42",
		"-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut, "-pubkey", r.pub, "-sth", r.head)
	return append(a, extra...)
}

func digestOf(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if d, ok := strings.CutPrefix(l, "plan digest: "); ok {
			return d
		}
	}
	t.Fatalf("no plan digest in %q", out)
	return ""
}

func (r *rig) content() string {
	s, err := sqlite.OpenReadOnly(r.db)
	if err != nil {
		r.t.Fatal(err)
	}
	defer s.Close()
	var all strings.Builder
	for e, err := range s.Load(context.Background(), "r1", -1) {
		if err != nil {
			r.t.Fatal(err)
		}
		all.Write(e.Data)
	}
	return all.String()
}

func TestDryRunListsAndChangesNothing(t *testing.T) {
	r := newRig(t)
	before := r.content()
	code, out, errb := r.run(r.base()...)
	if code != 0 {
		t.Fatalf("dry run: %d %s", code, errb)
	}
	for _, want := range []string{"run r1:", "replace", "run:start", "keep", "unchanged", "plan digest:", "dry run: nothing was changed", "not reached by this command", "Postgres dead tuples", "event store", "end marker not selected by the Payloads preset: name run:complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"Alice", "4111-1111"} {
		if strings.Contains(out, secret) {
			t.Errorf("the dry run printed content (%q)", secret)
		}
	}
	if r.content() != before {
		t.Error("a dry run changed the journal")
	}
}

func TestExecuteErasesAndIsAccountable(t *testing.T) {
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	d := digestOf(t, out)
	code, out, errb := r.run(r.execArgs(d)...)
	if code != 0 || !strings.Contains(out, "DONE") {
		t.Fatalf("execute: %d\n%s\n%s", code, out, errb)
	}
	if c := r.content(); strings.Contains(c, "Alice") || strings.Contains(c, "4111-1111") {
		t.Fatalf("content remains: %s", c)
	}
	// bide audit redactions passes on the redacted database with the head the command wrote.
	b, err := os.ReadFile(r.headOut)
	if err != nil || len(bytes.Split(bytes.TrimSpace(b), []byte("\n"))) != 1 {
		t.Fatalf("head file = %q, %v; want one head", b, err)
	}
	sth := filepath.Join(r.dir, "trail.sth")
	os.WriteFile(sth, b, 0o644)
	auditcli.Stores = &auditcli.StoreOpeners{SQLite: func(path string) (auditcli.NotifyStores, error) {
		s, err := sqlite.OpenReadOnly(path)
		if err != nil {
			return auditcli.NotifyStores{}, err
		}
		return auditcli.NotifyStores{Journal: s, Events: s.EventStore(), Close: s.Close}, nil
	}}
	t.Cleanup(func() { auditcli.Stores = nil })
	var ao, ae bytes.Buffer
	if code := auditcli.Main("test", []string{"redactions", "-db", r.db, "-sth", sth, "-pubkey", r.pub, "-deployment", "test"}, &ao, &ae); code != 0 || !strings.Contains(ao.String(), "PASS") {
		t.Fatalf("bide audit redactions = %d\n%s\n%s", code, ao.String(), ae.String())
	}
	// A repeat of the same plan finds it done.
	_, out, _ = r.run(r.base()...)
	if !strings.Contains(out, "already redacted") {
		t.Errorf("the plan after the erasure does not say the records are already redacted:\n%s", out)
	}
}

func TestRefusals(t *testing.T) {
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	d := digestOf(t, out)
	// A digest that differs from d in its first character (not "00" + d[2:], which equals d
	// whenever d starts with "00").
	wrong := []byte(d)
	if wrong[0] == '0' {
		wrong[0] = '1'
	} else {
		wrong[0] = '0'
	}
	cases := []struct {
		name string
		args []string
		want int
		msg  string
	}{
		{"no selection", []string{"-db", r.db, "-run", "r1"}, 2, "select records"},
		{"no run", []string{"-db", r.db, "-payloads"}, 2, "-run is required"},
		{"both stores", []string{"-db", r.db, "-postgres-env", "X", "-run", "r1", "-payloads"}, 2, "one of -db and -postgres-env"},
		{"a dsn as an argument", append(r.base(), "postgres://u:p@h/db"), 2, "never an argument"},
		{"execute without a reason", append(r.base(), "-execute", "-plan-digest", d), 2, "-execute needs"},
		{"execute flags without -execute", append(r.base(), "-reason", "x"), 2, "go with -execute"},
		{"execute without a head", append(r.base(), "-execute", "-plan-digest", d, "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut), 2, "signed journal head"},
		{"wrong digest", r.execArgs(string(wrong)), 1, "not the one reviewed"},
		{"missing key", r.execArgs(d, "-sign-key", filepath.Join(r.dir, "nope")), 1, "nope"},
		{"unknown run", []string{"-db", r.db, "-run", "nope", "-payloads", "-execute", "-plan-digest", "x", "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut, "-allow-uncovered"}, 1, "not over"},
	}
	for _, tc := range cases {
		before := r.content()
		code, out, errb := r.run(tc.args...)
		if code != tc.want || !strings.Contains(errb+out, tc.msg) {
			t.Errorf("%s: %d, want %d containing %q\n%s%s", tc.name, code, tc.want, tc.msg, out, errb)
		}
		if strings.Contains(errb, "p@h") || strings.Contains(errb, ":p@") {
			t.Errorf("%s: the DSN is echoed: %s", tc.name, errb)
		}
		if r.content() != before {
			t.Errorf("%s: a refused command changed the journal", tc.name)
		}
	}
	if _, err := os.Stat(r.headOut); err == nil {
		t.Error("a refused command wrote the head file")
	}
	// A head of the wrong key is not a covering head.
	other := filepath.Join(r.dir, "other.key")
	var o, e bytes.Buffer
	approvecmd.KeygenMain([]string{"-out", other, "-color", "never"}, &o, &e)
	if code, _, errb := r.run(r.execArgs(d, "-pubkey", other+".pub")...); code != 1 || !strings.Contains(errb, "no signed journal head") {
		t.Errorf("a head the wrong key signed: %d %s", code, errb)
	}
	if strings.Contains(r.content(), "redacted") {
		t.Error("a refused command erased")
	}
	// An allowed uncovered erasure is recorded.
	if code, _, errb := r.run(append(r.base(), "-execute", "-plan-digest", d, "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut, "-allow-uncovered")...); code != 0 {
		t.Fatalf("allowed uncovered: %d %s", code, errb)
	}
}

func TestKeyFileMustBePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	d := digestOf(t, out)
	if err := os.Chmod(r.key, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := r.run(r.execArgs(d)...); code != 1 || strings.Contains(errb, "BEGIN") {
		t.Fatalf("a world-readable key: %d %s", code, errb)
	}
}

func TestSubRunsAreListedAndRefusedWhileParentLive(t *testing.T) {
	r := newRig(t)
	s, _ := sqlite.Open(r.db)
	defer s.Close()
	child := agent.SubRunID("r1", "c1")
	b, _ := agent.JournalEntry("run:complete", agent.Record{Kind: agent.StepValue, Result: []byte(`"x"`)})
	// A parent that is not over: a run with a record and no end marker.
	live := agenttest.MustJournal(s)
	if _, err := live.Step(context.Background(), "p2", "x", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	c2 := agent.SubRunID("p2", "c1")
	if _, err := agenttest.MustJournal(s).Step(context.Background(), c2, "x", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	s.Insert(context.Background(), c2, "run:complete", b)
	_ = child
	code, out, _ := r.run("-db", r.db, "-run", "p2", "-subruns", "-payloads")
	if code != 0 || !strings.Contains(out, "run "+c2) || !strings.Contains(out, "REFUSED") {
		t.Fatalf("dry run with -subruns: %d\n%s", code, out)
	}
}

func TestHelpAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main(nil, &out, &errb); code != 2 {
		t.Errorf("no verb: %d", code)
	}
	if code := Main([]string{"nope"}, &out, &errb); code != 2 {
		t.Errorf("unknown verb: %d", code)
	}
	if code := Main([]string{"redact", "-h"}, &out, &errb); code != 0 || !strings.Contains(errb.String(), "-plan-digest") {
		t.Errorf("redact -h: %d %s", code, errb.String())
	}
}

func TestLiveRunAndEventCopiesAreRefused(t *testing.T) {
	r := newRig(t)
	s, err := sqlite.Open(r.db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := agenttest.MustJournal(s).Step(ctx, "live", "x", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	s.Close()
	code, out, _ := r.run("-db", r.db, "-run", "live", "-payloads")
	if code != 0 || !strings.Contains(out, "NOT over") || !strings.Contains(out, "REFUSED") {
		t.Fatalf("dry run of a live run: %d\n%s", code, out)
	}
	args := []string{"-db", r.db, "-run", "live", "-payloads", "-execute", "-plan-digest", digestOf(t, out), "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut, "-allow-uncovered"}
	if code, _, errb := r.run(args...); code != 1 || !strings.Contains(errb, "refused") {
		t.Fatalf("execute on a live run: %d %s", code, errb)
	}

	// An event copy of r1 blocks the erasure unless asked.
	s, _ = sqlite.Open(r.db)
	leaf := []byte("bide.audit.event-leaf.v3\x00{}")
	if err := s.EventStore().Append(ctx, "r1", 0, leaf); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, out, _ = r.run(r.base()...)
	d := digestOf(t, out)
	if code, _, errb := r.run(r.execArgs(d)...); code != 1 || !strings.Contains(errb, "event store holds") {
		t.Fatalf("with event copies: %d %s", code, errb)
	}
	if strings.Contains(r.content(), `"redacted"`) {
		t.Fatal("erased despite event copies")
	}
	if code, _, errb := r.run(r.execArgs(d, "-keep-event-copies")...); code != 0 {
		t.Fatalf("-keep-event-copies: %d %s", code, errb)
	}
}

func TestDeploymentIsRequired(t *testing.T) {
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	d := digestOf(t, out)
	args := append(r.base(), "-execute", "-plan-digest", d, "-reason", "r", "-ticket", "t", "-sign-key", r.key, "-head-out", r.headOut, "-pubkey", r.pub, "-sth", r.head)
	if code, _, errb := r.run(args...); code != 2 || !strings.Contains(errb, "-deployment") {
		t.Fatalf("without -deployment: %d %s", code, errb)
	}
	if code, _, errb := r.run(append(args, "-deployment", "  ")...); code != 2 {
		t.Fatalf("with a blank -deployment: %d %s", code, errb)
	}
}

// With another connection holding the file open, the erased bytes are in neither the database file
// nor the WAL once -execute returns; and with a reader holding a transaction open the command says the
// storage was not compacted and exits 1, rather than claiming success.
func TestExecuteLeavesNoBytesInTheFiles(t *testing.T) {
	r := newRig(t)
	hold, err := sqlite.OpenReadOnly(r.db)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	if _, _, err := hold.Get(context.Background(), "r1", "run:start"); err != nil {
		t.Fatal(err)
	}
	if in, _ := fileHolds(t, r.db, "Alice"); !in {
		t.Fatal("the content is not in the database file before the erasure; the test cannot tell")
	}
	_, out, _ := r.run(r.base()...)
	code, out, errb := r.run(r.execArgs(digestOf(t, out))...)
	if code != 0 || !strings.Contains(out, "storage: wal_checkpoint(TRUNCATE)") {
		t.Fatalf("execute: %d\n%s%s", code, out, errb)
	}
	for _, secret := range []string{"Alice", "4111-1111"} {
		if inDB, inWAL := fileHolds(t, r.db, secret); inDB || inWAL {
			t.Errorf("%q is still in the database file (%v) or the WAL (%v) after -execute", secret, inDB, inWAL)
		}
	}
}

func fileHolds(t *testing.T, path, marker string) (inDB, inWAL bool) {
	t.Helper()
	db, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wal, err := os.ReadFile(path + "-wal")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return bytes.Contains(db, []byte(marker)), bytes.Contains(wal, []byte(marker))
}

func TestABusyCheckpointIsReportedNotClaimedAsSuccess(t *testing.T) {
	r := newRig(t)
	other, err := sql.Open("sqlite", r.db)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(context.Background(), `SELECT data FROM bide_steps`)
	if err != nil || !rows.Next() {
		t.Fatalf("reader: %v", err)
	}
	defer rows.Close()
	_, out, _ := r.run(r.base()...)
	code, out, errb := r.run(r.execArgs(digestOf(t, out))...)
	if code != 1 || !strings.Contains(out, "STORAGE NOT COMPACTED") || !strings.Contains(errb, "not compacted") {
		t.Fatalf("a reader held the file: exit %d\n%s%s", code, out, errb)
	}
	if strings.Contains(out, "DONE") {
		t.Errorf("the report says DONE although the storage was not compacted:\n%s", out)
	}
}

// A trail that already holds records and has no earlier head to attest them is refused, unless the
// operator says it knows.
func TestATrailWithRecordsAndNoEarlierHeadIsRefused(t *testing.T) {
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	d := digestOf(t, out)
	if code, _, errb := r.run(r.execArgs(d)...); code != 0 {
		t.Fatalf("first erasure: %d %s", code, errb)
	}
	// A second erasure (another selection) with the heads file gone.
	os.Remove(r.headOut)
	args := []string{"-db", r.db, "-run", "r1", "-name", "run:start"}
	_, out, _ = r.run(args...)
	d2 := digestOf(t, out)
	exec := append(args, "-execute", "-plan-digest", d2, "-reason", "again", "-ticket", "T-2", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut, "-pubkey", r.pub, "-sth", r.head, "-allow-uncovered")
	if code, _, errb := r.run(exec...); code != 1 || !strings.Contains(errb, "no earlier signed head") {
		t.Fatalf("without the earlier heads: %d %s", code, errb)
	}
	if code, _, errb := r.run(append(exec, "-accept-unheaded-trail")...); code != 0 {
		t.Fatalf("with -accept-unheaded-trail: %d %s", code, errb)
	}
}

// A run whose lease is live is refused before anything is written to the trail.
func TestALeasedRunWritesNothing(t *testing.T) {
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	d := digestOf(t, out)
	s, err := sqlite.Open(r.db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ok, err := s.AcquireLease(context.Background(), "r1", "driver", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if code, _, errb := r.run(r.execArgs(d)...); code != 1 || !strings.Contains(errb, "lease") {
		t.Fatalf("leased run: %d %s", code, errb)
	}
	leaves, err := s.EventStore().Load(context.Background(), audit.RedactionTrailName)
	if err != nil || len(leaves) != 0 {
		t.Fatalf("a refused attempt wrote %d trail record(s) (%v)", len(leaves), err)
	}
}

func TestRedactDSNHandlesAnEmptyPassword(t *testing.T) {
	dsn := "postgres://u:@h/db"
	if got := redactDSN("connect "+dsn+" failed: x", dsn); strings.Contains(got, "<password>") || !strings.Contains(got, "failed: x") {
		t.Errorf("redactDSN with an empty password = %q", got)
	}
	dsn = "postgres://u:hunter2@h/db"
	if got := redactDSN("dial hunter2@h: "+dsn, dsn); strings.Contains(got, "hunter2") {
		t.Errorf("the password leaks: %q", got)
	}
}

// A typo in -trail-db starts a new, empty trail; the earlier heads in -head-out do not match it, so the
// command refuses before it erases anything.
func TestAWrongTrailDatabaseIsRefusedBeforeErasing(t *testing.T) {
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	if code, _, errb := r.run(r.execArgs(digestOf(t, out))...); code != 0 {
		t.Fatalf("first erasure: %d %s", code, errb)
	}
	wrong := filepath.Join(r.dir, "typo.db")
	args := []string{"-db", r.db, "-run", "r1", "-prefix", "@"}
	_, out, _ = r.run(args...)
	exec := append(args, "-execute", "-plan-digest", digestOf(t, out), "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut, "-pubkey", r.pub, "-sth", r.head, "-allow-uncovered", "-trail-db", wrong)
	code, _, errb := r.run(exec...)
	if code != 1 || !strings.Contains(errb, "does not match its signed heads") {
		t.Fatalf("with a trail that the earlier heads do not match: %d %s", code, errb)
	}
	if leaves, err := func() ([][]byte, error) {
		w, err := sqlite.Open(wrong)
		if err != nil {
			return nil, err
		}
		defer w.Close()
		return w.EventStore().Load(context.Background(), audit.RedactionTrailName)
	}(); err != nil || len(leaves) != 0 {
		t.Fatalf("the refused attempt wrote %d record(s) to the wrong trail (%v)", len(leaves), err)
	}
}

// A refusal before the first trail record writes no head: there is nothing new to attest.
func TestARefusalBeforeTheTrailWritesNoHead(t *testing.T) {
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	d := digestOf(t, out)
	s, err := sqlite.Open(r.db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The head on file is signed by another key than -pubkey names, so none covers the run: RedactJournal
	// refuses after the trail check and before its first record.
	other := filepath.Join(r.dir, "other.key")
	var o, e bytes.Buffer
	approvecmd.KeygenMain([]string{"-out", other, "-color", "never"}, &o, &e)
	args := append(r.base(), "-execute", "-plan-digest", d, "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut, "-pubkey", other+".pub", "-sth", r.head)
	if code, _, errb := r.run(args...); code != 1 || !strings.Contains(errb, "no signed journal head") || !strings.Contains(errb, "nothing was erased or written") {
		t.Fatalf("refusal: %d %s", code, errb)
	}
	if _, err := os.Stat(r.headOut); err == nil {
		t.Error("a refused command wrote a head")
	}
	leaves, _ := s.EventStore().Load(context.Background(), audit.RedactionTrailName)
	if len(leaves) != 0 {
		t.Errorf("a refused command wrote %d trail record(s)", len(leaves))
	}
}

// Running the same plan again after an erasure, to finish compaction a reader had blocked: with nothing
// left to erase, a checkpoint that is still busy is reported, not called success.
func TestNothingToEraseStillReportsABusyCheckpoint(t *testing.T) {
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	if code, _, errb := r.run(r.execArgs(digestOf(t, out))...); code != 0 {
		t.Fatalf("erase: %d %s", code, errb)
	}
	other, err := sql.Open("sqlite", r.db)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(context.Background(), `SELECT data FROM bide_steps`)
	if err != nil || !rows.Next() {
		t.Fatalf("reader: %v", err)
	}
	defer rows.Close()
	// Something else writes while the reader holds its snapshot, so the log has frames the checkpoint
	// cannot finish with.
	w, err := sqlite.Open(r.db)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := w.AcquireLease(context.Background(), "other-run", "h", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	w.Close()
	// A selection that matches no record: no trail record is written, only the compaction runs.
	none := []string{"-db", r.db, "-run", "r1", "-name", "no-such-record"}
	_, out, _ = r.run(none...)
	args := append(none, "-execute", "-plan-digest", digestOf(t, out), "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut, "-pubkey", r.pub, "-sth", r.head)
	code, out, errb := r.run(args...)
	if code != 1 || !strings.Contains(out, "STORAGE NOT COMPACTED") || !strings.Contains(errb, "not compacted") {
		t.Fatalf("nothing to erase, checkpoint busy: exit %d\n%s%s", code, out, errb)
	}
}

// Naming the end marker erases it with the other payloads, and bide audit redactions then says how the
// run ended, from the signed trail: the journal itself says only that the run is over.
func TestErasedEndMarkerIsReportedPerTrail(t *testing.T) {
	r := newRig(t)
	named := append(r.base(), "-name", "run:complete")
	_, out, _ := r.run(named...)
	if !strings.Contains(out, "run:complete") || strings.Contains(out, "end marker not selected") {
		t.Fatalf("a dry run naming run:complete does not list it for replacement:\n%s", out)
	}
	code, out, errb := r.run(r.execArgs(digestOf(t, out), "-name", "run:complete")...)
	if code != 0 || !strings.Contains(out, "DONE") {
		t.Fatalf("execute: %d\n%s\n%s", code, out, errb)
	}
	b, err := os.ReadFile(r.headOut)
	if err != nil {
		t.Fatal(err)
	}
	sth := filepath.Join(r.dir, "trail.sth")
	os.WriteFile(sth, b, 0o644)
	auditcli.Stores = &auditcli.StoreOpeners{SQLite: func(path string) (auditcli.NotifyStores, error) {
		s, err := sqlite.OpenReadOnly(path)
		if err != nil {
			return auditcli.NotifyStores{}, err
		}
		return auditcli.NotifyStores{Journal: s, Events: s.EventStore(), Close: s.Close}, nil
	}}
	t.Cleanup(func() { auditcli.Stores = nil })
	var ao, ae bytes.Buffer
	if code := auditcli.Main("test", []string{"redactions", "-db", r.db, "-sth", sth, "-pubkey", r.pub, "-deployment", "test"}, &ao, &ae); code != 0 || !strings.Contains(ao.String(), "PASS") {
		t.Fatalf("bide audit redactions = %d\n%s\n%s", code, ao.String(), ae.String())
	}
	if !strings.Contains(ao.String(), "run r1: ended: completed (per trail)") {
		t.Errorf("the audit does not say how the run ended, per the trail:\n%s", ao.String())
	}
}
