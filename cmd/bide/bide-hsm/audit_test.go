package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/fakekms"
	"github.com/bide-ai/bide/signer/awskms"
	"github.com/bide-ai/bide/store/sqlite"
)

const kmsEd2 = "arn:aws:kms:us-east-1:111122223333:key/7777abcd-12ab-34cd-56ef-1234567890ab"
const kmsLog = "arn:aws:kms:us-east-1:111122223333:key/5555abcd-12ab-34cd-56ef-1234567890ab"

// auditEnv is an audited kill switch whose stop was signed by a KMS key, and a second KMS key that is the log key.
type auditEnv struct {
	t       *testing.T
	kms     *fakekms.KMS
	opens   atomic.Int32 // how many times a KMS client was asked for: a key was opened
	db      string
	policy  string
	opsRef  string
	logRef  string
	logPub  string
	heads   string
	stopArg []string
}

func newAuditEnv(t *testing.T) *auditEnv {
	t.Helper()
	e := &auditEnv{t: t, kms: fakekms.New(), opsRef: "awskms:" + kmsEd, logRef: "awskms:" + kmsLog}
	e.kms.AddEd25519(kmsEd)
	e.kms.AddEd25519(kmsLog)
	entry := enrol(t, e.kms, "ops", e.opsRef)
	e.logPub = enrol(t, e.kms, "log", e.logRef)
	v, err := audit.ParsePublicKey(entry)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	e.db = filepath.Join(dir, "j.db")
	s, err := sqlite.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	doc := agent.StopPolicyDoc{DeploymentID: "prod-91c2", Approvers: map[string]agent.StopApprover{"ops": {Alg: v.Alg(), Key: hex.EncodeToString(v.(interface{ PublicKey() []byte }).PublicKey())}}, Setters: []string{"ops"}, Lifters: []string{"ops"}, LiftQuorum: 1}
	b, _ := json.Marshal(doc)
	e.policy = filepath.Join(dir, "policy.json")
	if err := os.WriteFile(e.policy, b, 0o600); err != nil {
		t.Fatal(err)
	}
	e.heads = filepath.Join(dir, "heads.jsonl")
	return e
}

// run runs bide-hsm with a KMS client that counts how often a key is opened.
func (e *auditEnv) run(args ...string) result {
	e.t.Helper()
	d := deps{kms: func(ctx context.Context, region string) (awskms.API, error) {
		e.opens.Add(1)
		return e.kms.Client(ctx, region)
	}}
	return hsmWith(e.t, d, "", args...)
}

func (e *auditEnv) stop(args ...string) {
	e.t.Helper()
	r := e.run(append([]string{"stop", "-db", e.db, "-stop-policy", e.policy, "-as", "ops", "-key", e.opsRef, "-yes"}, args...)...)
	if r.code != 0 {
		e.t.Fatalf("stop %v: %d\n%s", args, r.code, r.all())
	}
}

func (e *auditEnv) audit(extra ...string) result {
	e.t.Helper()
	return e.run(append([]string{"audit", "stops", "-db", e.db, "-stop-policy", e.policy, "-pubkey", e.logPub}, extra...)...)
}

func (e *auditEnv) sign(extra ...string) result {
	e.t.Helper()
	return e.audit(append([]string{"-sign-head", "-log-key", e.logRef, "-head-out", e.heads}, extra...)...)
}

func (e *auditEnv) headLines() int {
	e.t.Helper()
	b, err := os.ReadFile(e.heads)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(b)), "\n"))
}

// bide-hsm audit stops -sign-head signs the stop trail's head with a log key in a KMS: the head verifies with the
// existing verifier, a run with nothing new signs nothing and opens no key, and a new record is unattested until
// the next run.
func TestAuditStopsSignHeadWithAKMSKey(t *testing.T) {
	e := newAuditEnv(t)
	e.stop("-tool", "refund", "-reason", "provider incident")
	if r := e.audit(); r.code != 1 || !strings.Contains(r.out, "no signed head") {
		t.Fatalf("an unattested trail: %d\n%s", r.code, r.all())
	}
	before := e.opens.Load()
	r := e.sign()
	if r.code != 0 || !strings.Contains(r.out, "appended it to") || e.headLines() != 1 || e.opens.Load() == before {
		t.Fatalf("sign-head: %d, %d head(s)\n%s", r.code, e.headLines(), r.all())
	}
	if r := e.audit("-sth", e.heads); r.code != 0 || !strings.Contains(r.out, "PASS") {
		t.Fatalf("the KMS-signed head must make the audit pass: %d\n%s", r.code, r.all())
	}
	// Nothing new: nothing signed, and the key is not opened.
	before = e.opens.Load()
	if r := e.sign(); r.code != 0 || !strings.Contains(r.out, "nothing signed") || e.headLines() != 1 || e.opens.Load() != before {
		t.Fatalf("no new records: %d, %d head(s), %d key opens\n%s", r.code, e.headLines(), e.opens.Load()-before, r.all())
	}
	// A new record is unattested until the next run.
	e.stop("-lift", e.activeID(), "-reason", "recovered")
	if r := e.audit("-sth", e.heads); r.code != 1 || !strings.Contains(r.out, "not attested") {
		t.Fatalf("a record after the head: %d\n%s", r.code, r.all())
	}
	if r := e.sign(); r.code != 0 || e.headLines() != 2 {
		t.Fatalf("second sign-head: %d, %d head(s)\n%s", r.code, e.headLines(), r.all())
	}
	if r := e.audit("-sth", e.heads); r.code != 0 {
		t.Fatalf("the audit after the second head: %d\n%s", r.code, r.all())
	}
}

// activeID is the ID of the one stop in force.
func (e *auditEnv) activeID() string {
	e.t.Helper()
	s, err := sqlite.Open(e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	defer s.Close()
	j, _ := agent.NewJournal(s)
	var doc agent.StopPolicyDoc
	b, _ := os.ReadFile(e.policy)
	_ = json.Unmarshal(b, &doc)
	pol, err := audit.StopPolicyFromDoc(doc)
	if err != nil {
		e.t.Fatal(err)
	}
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(pol), agent.WithStopVerifier(audit.StopVerifierFromAudit))
	if err != nil {
		e.t.Fatal(err)
	}
	st, err := ctl.State(context.Background())
	if err != nil || len(st.Active()) != 1 {
		e.t.Fatalf("state: %v, %d active", err, len(st.Active()))
	}
	return st.Active()[0].ID
}

// What does not verify is not signed, and no key is opened for it: a trail that does not verify under the pinned
// policy, a log key that is not the key -pubkey names, and a head file that claims more than the trail holds.
func TestAuditStopsSignHeadSignsOnlyWhatVerified(t *testing.T) {
	e := newAuditEnv(t)
	e.stop("-all", "-reason", "incident")

	// Another deployment's policy: the record does not verify under it.
	other := newAuditEnv(t)
	before := e.opens.Load()
	e2 := e.run("audit", "stops", "-db", e.db, "-stop-policy", other.policy, "-pubkey", e.logPub, "-sign-head", "-log-key", e.logRef, "-head-out", e.heads)
	if e2.code != 1 || e.headLines() != 0 || e.opens.Load() != before {
		t.Fatalf("a trail that does not verify: %d, %d head(s), %d key opens\n%s", e2.code, e.headLines(), e.opens.Load()-before, e2.all())
	}

	// The log key is not the key -pubkey names: a head under it would not verify.
	e.kms.AddEd25519(kmsEd2)
	r := e.run("audit", "stops", "-db", e.db, "-stop-policy", e.policy, "-pubkey", e.logPub, "-sign-head", "-log-key", "awskms:"+kmsEd2, "-head-out", e.heads)
	if r.code != 3 || !strings.Contains(r.errOut, "not the key -pubkey names") || e.headLines() != 0 {
		t.Fatalf("the wrong log key: %d, %d head(s)\n%s", r.code, e.headLines(), r.all())
	}
}

// Usage, and nothing a key reference can hold is echoed or accepted: only audit stops is offered, a PIN in the
// reference is refused without being printed, a flag of another key kind is refused, and the messages name this
// command.
func TestAuditStopsUsageAndSecrets(t *testing.T) {
	e := newAuditEnv(t)
	e.stop("-all", "-reason", "incident")
	if r := e.run("audit"); r.code != 2 || !strings.Contains(r.errOut, "bide-hsm audit stops") {
		t.Fatalf("bare: %d\n%s", r.code, r.all())
	}
	r := e.run("audit", "verify", "-bundle", "x", "awskms:arn:aws:kms:us-east-1:1:key/secret-looking")
	if r.code != 2 || strings.Contains(r.all(), "secret-looking") || !strings.Contains(r.errOut, "the one verb is stops") {
		t.Fatalf("another verb: %d\n%s", r.code, r.all())
	}
	for _, tc := range []struct{ name, ref, want string }{
		{"pin in the reference", "pkcs11:token=t;object=k?module-path=/lib/x.so&pin-value=hunter2-secret", "pin-value"},
		{"no scheme", "hunter2-secret", "no scheme"},
	} {
		r := e.sign("-log-key", tc.ref)
		if r.code != 3 || e.headLines() != 0 || strings.Contains(r.all(), "hunter2-secret") {
			t.Errorf("%s: %d, %d head(s)\n%s", tc.name, r.code, e.headLines(), r.all())
		}
		if !strings.Contains(r.all(), tc.want) {
			t.Errorf("%s: want %q in\n%s", tc.name, tc.want, r.all())
		}
	}
	if r := e.sign("-allow-extractable"); r.code != 3 || !strings.Contains(r.errOut, "-allow-extractable applies to a PKCS#11 key only") || e.headLines() != 0 {
		t.Fatalf("-allow-extractable on a KMS key: %d\n%s", r.code, r.all())
	}
	// The flags go with -sign-head, and the help names this command.
	if r := e.audit("-log-key", e.logRef); r.code != 2 || !strings.Contains(r.errOut, "bide-hsm audit stops: -log-key and -head-out go with -sign-head") {
		t.Fatalf("-log-key without -sign-head: %d\n%s", r.code, r.all())
	}
	if r := e.run("audit", "stops", "-h"); !strings.Contains(r.errOut, "reference to the log key in an HSM or KMS") {
		t.Fatalf("help: %d\n%s", r.code, r.all())
	}
}
