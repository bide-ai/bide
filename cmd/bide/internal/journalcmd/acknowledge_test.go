package journalcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/auditcli"
	"github.com/bide-ai/bide/store/sqlite"
)

func (r *rig) acknowledge(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(append([]string{"acknowledge", "-color", "never"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func (r *rig) plantJunk() {
	r.t.Helper()
	s, err := sqlite.Open(r.db)
	if err != nil {
		r.t.Fatal(err)
	}
	defer s.Close()
	leaves, _ := s.EventStore().Load(context.Background(), audit.RedactionTrailName)
	if err := s.EventStore().Append(context.Background(), audit.RedactionTrailName, len(leaves), []byte("planted junk")); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) auditRedactions(headFiles ...string) (int, string) {
	r.t.Helper()
	auditcli.Stores = &auditcli.StoreOpeners{SQLite: func(path string) (auditcli.NotifyStores, error) {
		s, err := sqlite.OpenReadOnly(path)
		if err != nil {
			return auditcli.NotifyStores{}, err
		}
		return auditcli.NotifyStores{Journal: s, Events: s.EventStore(), Close: s.Close}, nil
	}}
	r.t.Cleanup(func() { auditcli.Stores = nil })
	args := []string{"redactions", "-db", r.db, "-deployment", "test", "-pubkey", r.pub}
	for _, h := range headFiles {
		args = append(args, "-sth", h)
	}
	var ao, ae bytes.Buffer
	code := auditcli.Main("test", args, &ao, &ae)
	return code, ao.String() + ae.String()
}

func (r *rig) headFiles() []string {
	r.t.Helper()
	b, err := os.ReadFile(r.headOut)
	if err != nil {
		r.t.Fatal(err)
	}
	var files []string
	for i, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		f := filepath.Join(r.dir, "h"+string(rune('0'+i))+".sth")
		os.WriteFile(f, line, 0o644)
		files = append(files, f)
	}
	return files
}

func (r *rig) ackArgs(pos string, extra ...string) []string {
	return append([]string{"-db", r.db, "-position", pos, "-reason", "investigated: a stray write by a maintenance script", "-ticket", "SEC-7",
		"-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut}, extra...)
}

func TestAcknowledgeAPlantedLeafMakesTheAuditPass(t *testing.T) {
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	if code, _, errb := r.run(r.execArgs(digestOf(t, out))...); code != 0 {
		t.Fatalf("erase: %d %s", code, errb)
	}
	r.plantJunk() // position 2: the begin and end are at 0 and 1
	code, text := r.auditRedactions(r.headFiles()...)
	if code != 1 || !strings.Contains(text, "UNATTESTED_TAIL") && !strings.Contains(text, "MALFORMED") {
		t.Fatalf("with a planted leaf: %d\n%s", code, text)
	}
	if code, _, errb := r.acknowledge(r.ackArgs("2")...); code != 0 {
		t.Fatalf("acknowledge: %d %s", code, errb)
	}
	code, text = r.auditRedactions(r.headFiles()...)
	if code != 0 || !strings.Contains(text, "acknowledged") || !strings.Contains(text, "PASS") {
		t.Fatalf("after the acknowledgement: %d\n%s", code, text)
	}
}

func TestAcknowledgeRefusals(t *testing.T) {
	r := newRig(t)
	_, out, _ := r.run(r.base()...)
	if code, _, errb := r.run(r.execArgs(digestOf(t, out))...); code != 0 {
		t.Fatalf("erase: %d %s", code, errb)
	}
	for name, tc := range map[string]struct {
		args []string
		want int
		msg  string
	}{
		"a record the tool wrote": {r.ackArgs("0"), 1, "not a finding"},
		"outside the trail":       {r.ackArgs("99"), 1, "outside the trail"},
		"no position":             {[]string{"-db", r.db, "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut}, 2, "-position"},
		"no deployment":           {[]string{"-db", r.db, "-position", "2", "-reason", "r", "-ticket", "t", "-sign-key", r.key, "-head-out", r.headOut}, 2, "-deployment"},
		"no store":                {[]string{"-position", "2", "-reason", "r", "-ticket", "t", "-deployment", "test", "-sign-key", r.key, "-head-out", r.headOut}, 2, "store"},
		"a dsn as an argument":    {append(r.ackArgs("2"), "postgres://u:p@h/db"), 2, "never an argument"},
	} {
		code, _, errb := r.acknowledge(tc.args...)
		if code != tc.want || !strings.Contains(errb, tc.msg) {
			t.Errorf("%s: %d %q, want %d containing %q", name, code, errb, tc.want, tc.msg)
		}
	}
	// With the earlier heads gone the trail is unattested: refused until the operator says so.
	r.plantJunk()
	os.Remove(r.headOut)
	if code, _, errb := r.acknowledge(r.ackArgs("2")...); code != 1 || !strings.Contains(errb, "no earlier signed head") {
		t.Fatalf("without the earlier heads: %d %s", code, errb)
	}
}
