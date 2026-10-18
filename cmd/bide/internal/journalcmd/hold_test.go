package journalcmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/store/sqlite"
)

func (r *rig) hold(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(append([]string{"hold"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func (r *rig) placeArgs(extra ...string) []string {
	return append([]string{"place", "-db", r.db, "-id", "H-1", "-matter", "regulator inquiry 12", "-run", "r1", "-tree", "-reason", "inquiry opened", "-ticket", "LEGAL-5",
		"-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut}, extra...)
}

func (r *rig) liftArgs() []string {
	return []string{"lift", "-db", r.db, "-id", "H-1", "-reason", "inquiry closed", "-ticket", "LEGAL-9", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut}
}

func TestHoldPlaceListLiftAndTheErasureIsRefused(t *testing.T) {
	r := newRig(t)
	if code, out, errb := r.hold(r.placeArgs()...); code != 0 || !strings.Contains(out, "placed and recorded") {
		t.Fatalf("place: %d %s%s", code, out, errb)
	}
	code, out, _ := r.hold("list", "-db", r.db)
	if code != 0 || !strings.Contains(out, "H-1") || !strings.Contains(out, "regulator inquiry 12") || !strings.Contains(out, "r1") {
		t.Fatalf("list: %d %s", code, out)
	}
	// The dry run of an erasure names the hold and says it is refused; -execute refuses, writing nothing more.
	code, out, _ = r.run(r.base()...)
	if code != 0 || !strings.Contains(out, "REFUSED") || !strings.Contains(out, "H-1") || !strings.Contains(out, "regulator inquiry 12") {
		t.Fatalf("dry run under a hold: %d\n%s", code, out)
	}
	before := r.content()
	execArgs := append(r.base(), "-execute", "-plan-digest", digestOf(t, out), "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut, "-allow-uncovered")
	if code, _, errb := r.run(execArgs...); code != 1 || !strings.Contains(errb, "legal hold") {
		t.Fatalf("erase under a hold: %d %s", code, errb)
	}
	if r.content() != before {
		t.Fatal("a held run was erased")
	}
	// bide audit redactions against the database: the hold is listed and the audit passes.
	if code, text := r.auditRedactions(r.headFiles()...); code != 0 || !strings.Contains(text, "legal hold H-1 in force") {
		t.Fatalf("audit with a hold: %d\n%s", code, text)
	}
	if code, out, errb := r.hold(r.liftArgs()...); code != 0 || !strings.Contains(out, "lifted and recorded") {
		t.Fatalf("lift: %d %s%s", code, out, errb)
	}
	if code, out, _ := r.hold("list", "-db", r.db); code != 0 || !strings.Contains(out, "no legal hold") {
		t.Fatalf("list after the lift: %d %s", code, out)
	}
	_, out, _ = r.run(r.base()...)
	if strings.Contains(out, "REFUSED") {
		t.Fatalf("a lifted hold still refuses:\n%s", out)
	}
	if code, _, errb := r.run(r.execArgs(digestOf(t, out))...); code != 0 {
		t.Fatalf("erase after the lift: %d %s", code, errb)
	}
	if code, text := r.auditRedactions(r.headFiles()...); code != 0 {
		t.Fatalf("audit after the lift and the erasure: %d\n%s", code, text)
	}
}

func TestHoldRefusalsAndUsage(t *testing.T) {
	r := newRig(t)
	for name, tc := range map[string]struct {
		args []string
		want int
		msg  string
	}{
		"no verb":                {nil, 2, "usage"},
		"unknown verb":           {[]string{"freeze"}, 2, "unknown verb"},
		"place without a run":    {[]string{"place", "-db", r.db, "-id", "x", "-matter", "m", "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut}, 2, "-run"},
		"place without a matter": {[]string{"place", "-db", r.db, "-id", "x", "-run", "r1", "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut}, 2, "-matter"},
		"no store":               {[]string{"list"}, 2, "-db"},
		"a dsn as an argument":   {[]string{"list", "-db", r.db, "postgres://u:p@h/db"}, 2, "never an argument"},
		"lift of an unknown id":  {append([]string{}, r.liftArgs()...), 1, "shows a hold"},
	} {
		code, _, errb := r.hold(tc.args...)
		if code != tc.want || !strings.Contains(errb, tc.msg) {
			t.Errorf("%s: %d %q, want %d containing %q", name, code, errb, tc.want, tc.msg)
		}
	}
	if code, _, errb := r.hold(r.placeArgs()...); code != 0 {
		t.Fatal(errb)
	}
	if code, _, errb := r.hold(r.placeArgs()...); code != 1 || !strings.Contains(errb, "already active") {
		t.Errorf("placing an active ID again: %d %s", code, errb)
	}
}

// A hold lifted by hand in the database is found by bide audit redactions against it.
func TestAuditFindsAHoldLiftedByHand(t *testing.T) {
	r := newRig(t)
	if code, _, errb := r.hold(r.placeArgs()...); code != 0 {
		t.Fatal(errb)
	}
	s, err := sqlite.Open(r.db)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.LiftHold(context.Background(), "H-1"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	s.Close()
	code, text := r.auditRedactions(r.headFiles()...)
	if code != 1 || !strings.Contains(text, "HOLD_NOT_IN_STORE") {
		t.Fatalf("audit: %d\n%s", code, text)
	}
	var _ = audit.RedactionTrailName
	var _ = agent.ErrConfig
	var _ = os.Remove
}
