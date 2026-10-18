package journalcmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/internal/auditcli"
	"github.com/bide-ai/bide/store/sqlite"
)

// authorizerKey writes a second key file and returns its path and its public key text.
func (r *rig) authorizerKey() (path, pub string) {
	r.t.Helper()
	path = filepath.Join(r.dir, "authorizer.key")
	var out, errb bytes.Buffer
	if code := approvecmd.KeygenMain([]string{"-out", path, "-color", "never"}, &out, &errb); code != 0 {
		r.t.Fatalf("keygen: %d %s", code, errb.String())
	}
	s, err := approvecmd.LoadSigner(path)
	if err != nil {
		r.t.Fatal(err)
	}
	return path, audit.FormatPublicKey(s.Alg(), s.PublicKey())
}

func TestHoldLiftNeedsASecondPerson(t *testing.T) {
	r := newRig(t)
	if code, out, errb := r.hold(r.placeArgs()...); code != 0 {
		t.Fatalf("place: %d\n%s%s", code, out, errb)
	}
	keyPath, pub := r.authorizerKey()
	authz := filepath.Join(r.dir, "lift.auth")
	auth := []string{"authorize", "-db", r.db, "-id", "H-1", "-reason", "inquiry closed", "-ticket", "LEGAL-9", "-deployment", "test", "-trail-pubkey", r.pub, "-authorizer-key", keyPath, "-out", authz}

	// A dry run shows what would be signed and writes nothing.
	code, out, errb := r.hold(auth...)
	if code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, "regulator inquiry 12") {
		t.Fatalf("authorize dry run: %d\n%s%s", code, out, errb)
	}
	if _, err := os.Stat(authz); err == nil {
		t.Fatal("a dry run wrote the authorization")
	}
	// The trail's own key is refused as the authorizer.
	same := append(append([]string{}, auth[:len(auth)-4]...), "-authorizer-key", r.key, "-out", filepath.Join(r.dir, "same.auth"), "-execute")
	if code, _, _ = r.hold(same...); code == 0 {
		t.Fatal("authorize signed with the trail's own key")
	}
	if code, out, errb = r.hold(append(auth, "-execute")...); code != 0 {
		t.Fatalf("authorize: %d\n%s%s", code, out, errb)
	}
	if code, _, _ = r.hold(append(auth, "-execute")...); code == 0 {
		t.Fatal("authorize overwrote an existing file")
	}

	lift := append(r.liftArgs(), "-require-authorization", "-authorizer-pubkey", pub)
	if code, _, errb = r.hold(lift...); code != 2 || !strings.Contains(errb, "-authorization") {
		t.Fatalf("lift without -authorization = %d\n%s", code, errb)
	}
	// An authorization signed by a key that is not pinned is refused, and the hold stays.
	_, otherPub := r.authorizerKeyNamed("other.key")
	if code, _, errb = r.hold(append(r.liftArgs(), "-require-authorization", "-authorizer-pubkey", otherPub, "-authorization", authz)...); code != 1 || !strings.Contains(errb, "not one of the keys") {
		t.Fatalf("lift pinned to another authorizer = %d\n%s", code, errb)
	}
	if code, out, _ = r.hold("list", "-db", r.db); !strings.Contains(out, "H-1") {
		t.Fatalf("the hold was lifted by a refused lift: %s", out)
	}
	if code, out, errb = r.hold(append(lift, "-authorization", authz)...); code != 0 {
		t.Fatalf("lift: %d\n%s%s", code, out, errb)
	}
	if code, out, _ = r.hold("list", "-db", r.db); strings.Contains(out, "H-1") {
		t.Fatalf("the hold is still in force: %s", out)
	}
	// bide audit redactions, given the authorizer's key and requiring authorizations, passes.
	code, report := r.auditRedactionsWith(r.headFiles(), "-authorizer-pubkey", pub, "-require-lift-authorization")
	if code != 0 || !strings.Contains(report, "PASS") {
		t.Fatalf("audit = %d\n%s", code, report)
	}
	// Given another key to pin, the same trail fails with LIFT_UNAUTHORIZED.
	code, report = r.auditRedactionsWith(r.headFiles(), "-authorizer-pubkey", otherPub)
	if code == 0 || !strings.Contains(report, "LIFT_UNAUTHORIZED") {
		t.Fatalf("audit pinned to another authorizer = %d\n%s", code, report)
	}
}

func (r *rig) authorizerKeyNamed(name string) (path, pub string) {
	r.t.Helper()
	path = filepath.Join(r.dir, name)
	var out, errb bytes.Buffer
	if code := approvecmd.KeygenMain([]string{"-out", path, "-color", "never"}, &out, &errb); code != 0 {
		r.t.Fatalf("keygen: %d %s", code, errb.String())
	}
	s, err := approvecmd.LoadSigner(path)
	if err != nil {
		r.t.Fatal(err)
	}
	return path, audit.FormatPublicKey(s.Alg(), s.PublicKey())
}

func (r *rig) auditRedactionsWith(heads []string, extra ...string) (int, string) {
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
	for _, h := range heads {
		args = append(args, "-sth", h)
	}
	var ao, ae bytes.Buffer
	code := auditcli.Main("test", append(args, extra...), &ao, &ae)
	return code, ao.String() + ae.String()
}
