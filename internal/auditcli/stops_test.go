package auditcli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

type stopFixture struct {
	trail, sth, policy, pub, otherPub string
	dir                               string
}

func stopJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newStopFixture(t *testing.T) stopFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	pubs := map[string]ed25519.PublicKey{}
	privs := map[string]ed25519.PrivateKey{}
	for _, id := range []string{"ops1", "ops2", "risk"} {
		pub, priv, _ := ed25519.GenerateKey(nil)
		pubs[id], privs[id] = pub, priv
	}
	pol := agent.StopPolicy{DeploymentID: "prod-91c2", Setters: []string{"ops1"}, Lifters: []string{"ops1", "ops2", "risk"}, LiftQuorum: 2, LiftNeedsOther: true,
		Verifiers: func(id string) (agent.ApproverVerifier, bool) {
			p, ok := pubs[id]
			return audit.Ed25519Verifier{Pub: p}, ok
		}}
	events := audit.NewMemEventStore()
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(pol), agent.WithStopVerifier(audit.StopVerifierFromAudit), agent.WithStopTrail(audit.NewStopTrail(events)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := ctl.SetStop(ctx, agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: "refund"}, Reason: "incident", By: "ops1", Signer: audit.Ed25519Signer{Priv: privs["ops1"]}})
	if err != nil {
		t.Fatal(err)
	}
	for _, by := range []string{"ops2", "risk"} {
		if _, err := ctl.LiftStop(ctx, e.ID, agent.LiftRequest{By: by, Signer: audit.Ed25519Signer{Priv: privs[by]}}); err != nil {
			t.Fatal(err)
		}
	}
	hpub, hpriv, _ := ed25519.GenerateKey(nil)
	opub, _, _ := ed25519.GenerateKey(nil)
	x, err := audit.ExportStopTrail(ctx, events, agent.StopTrailName)
	if err != nil {
		t.Fatal(err)
	}
	head, err := audit.SignStopTrail(ctx, events, agent.StopTrailName, audit.Ed25519Signer{Priv: hpriv}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := pol.Doc()
	if err != nil {
		t.Fatal(err)
	}
	f := stopFixture{dir: dir, trail: filepath.Join(dir, "trail.json"), sth: filepath.Join(dir, "sth.json"), policy: filepath.Join(dir, "policy.json"),
		pub: hex.EncodeToString(hpub), otherPub: hex.EncodeToString(opub)}
	stopJSON(t, f.trail, x)
	stopJSON(t, f.sth, head)
	stopJSON(t, f.policy, doc)
	return f
}

func runStops(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main("", append([]string{"stops"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func TestStops_ExitStatuses(t *testing.T) {
	f := newStopFixture(t)
	for _, tc := range []struct {
		name string
		args []string
		want int
		has  string
	}{
		{"verifies", []string{"-trail", f.trail, "-sth", f.sth, "-pubkey", f.pub, "-stop-policy", f.policy}, 0, "PASS"},
		{"another head key", []string{"-trail", f.trail, "-sth", f.sth, "-pubkey", f.otherPub, "-stop-policy", f.policy}, 1, "does not verify"},
		{"no head", []string{"-trail", f.trail, "-pubkey", f.pub, "-stop-policy", f.policy}, 1, "no signed head"},
		{"unanchored", []string{"-trail", f.trail, "-unanchored", "-stop-policy", f.policy}, 0, "UNANCHORED"},
		{"no policy", []string{"-trail", f.trail, "-unanchored"}, 2, "missing -stop-policy"},
		{"nothing to read", []string{"-unanchored", "-stop-policy", f.policy}, 2, "missing -trail"},
		{"no key", []string{"-trail", f.trail, "-stop-policy", f.policy}, 2, "missing -pubkey"},
		{"sth without key", []string{"-trail", f.trail, "-sth", f.sth, "-unanchored", "-stop-policy", f.policy}, 2, "-sth needs -pubkey"},
		{"store needs the full build", []string{"-db", "x.db", "-unanchored", "-stop-policy", f.policy}, 2, "reads an exported trail only"},
	} {
		code, out, errb := runStops(tc.args...)
		if code != tc.want || !strings.Contains(out+errb, tc.has) {
			t.Errorf("%s: exit %d, want %d, containing %q:\n%s%s", tc.name, code, tc.want, tc.has, out, errb)
		}
	}
	// The report shows the stop and that it is lifted.
	_, out, _ := runStops("-trail", f.trail, "-sth", f.sth, "-pubkey", f.pub, "-stop-policy", f.policy)
	if !strings.Contains(out, "tool refund") || !strings.Contains(out, "lifted") {
		t.Errorf("report:\n%s", out)
	}
}

// A tampered trail, and a policy of another deployment, fail.
func TestStops_TamperAndWrongPolicy(t *testing.T) {
	f := newStopFixture(t)
	var x audit.StopTrailExport
	b, _ := os.ReadFile(f.trail)
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	x.Leaves = x.Leaves[:len(x.Leaves)-1] // the last lift deleted
	tampered := filepath.Join(f.dir, "tampered.json")
	stopJSON(t, tampered, x)
	if code, out, errb := runStops("-trail", tampered, "-sth", f.sth, "-pubkey", f.pub, "-stop-policy", f.policy); code != 1 || !strings.Contains(out+errb, "records were deleted") {
		t.Errorf("tampered: exit %d:\n%s%s", code, out, errb)
	}
	var doc agent.StopPolicyDoc
	pb, _ := os.ReadFile(f.policy)
	_ = json.Unmarshal(pb, &doc)
	doc.DeploymentID = "staging-7f3a"
	other := filepath.Join(f.dir, "other.json")
	stopJSON(t, other, doc)
	if code, out, errb := runStops("-trail", f.trail, "-sth", f.sth, "-pubkey", f.pub, "-stop-policy", other); code != 1 || !strings.Contains(out+errb, "do not verify") {
		t.Errorf("another deployment's policy: exit %d:\n%s%s", code, out, errb)
	}
	bad := filepath.Join(f.dir, "bad.json")
	doc.Setters = nil
	stopJSON(t, bad, doc)
	if code, _, _ := runStops("-trail", f.trail, "-unanchored", "-stop-policy", bad); code != 4 && code != 3 && code != 2 {
		t.Errorf("an unusable policy: exit %d", code)
	}
}
