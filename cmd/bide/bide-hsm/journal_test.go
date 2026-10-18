package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/fakekms"
	"github.com/bide-ai/bide/signer/awskms"
	"github.com/bide-ai/bide/store/sqlite"
)

const kmsAuthz = "arn:aws:kms:us-east-1:111122223333:key/6666abcd-12ab-34cd-56ef-1234567890ab"

// bide-hsm journal is bide journal with its keys in a KMS: the trail's key signs the hold's placement
// and lift, a second KMS key (the authorizer's) signs the lift's authorization, and the lift refuses
// without it. No key material is read, and the key references are never a file.
func TestJournalHoldLiftWithAuthorizationFromKMSKeys(t *testing.T) {
	kms := fakekms.New()
	kms.AddEd25519(kmsLog)
	kms.AddEd25519(kmsAuthz)
	logRef, authRef := "awskms:"+kmsLog, "awskms:"+kmsAuthz
	logPub := enrol(t, kms, "log", logRef)
	authPub := enrol(t, kms, "auth", authRef)
	dir := t.TempDir()
	db := filepath.Join(dir, "j.db")
	s, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	heads, authz := filepath.Join(dir, "heads.jsonl"), filepath.Join(dir, "lift.auth")
	d := deps{kms: func(ctx context.Context, region string) (awskms.API, error) { return kms.Client(ctx, region) }}
	journal := func(args ...string) result {
		t.Helper()
		var out, errb bytes.Buffer
		code := run(append([]string{"journal"}, args...), strings.NewReader(""), &out, &errb, d)
		return result{code: code, out: out.String(), errOut: errb.String()}
	}
	common := []string{"-db", db, "-id", "H-1", "-reason", "matter", "-ticket", "LEGAL-1", "-deployment", "prod"}
	if r := journal(append(append([]string{"hold", "place", "-matter", "litigation", "-run", "r1", "-sign-key", logRef, "-head-out", heads}, common...), "-color", "never")...); r.code != 0 {
		t.Fatalf("place: %d\n%s", r.code, r.all())
	}
	// The authorizer's key is a KMS key too; the dry run signs nothing.
	auth := []string{"hold", "authorize", "-db", db, "-id", "H-1", "-reason", "closed", "-ticket", "LEGAL-9", "-deployment", "prod", "-trail-pubkey", logPub, "-authorizer-key", authRef, "-out", authz, "-color", "never"}
	if r := journal(auth...); r.code != 0 || !strings.Contains(r.out, "dry run") {
		t.Fatalf("authorize dry run: %d\n%s", r.code, r.all())
	}
	if _, err := os.Stat(authz); err == nil {
		t.Fatal("a dry run wrote the authorization")
	}
	if r := journal(append(auth, "-execute")...); r.code != 0 {
		t.Fatalf("authorize: %d\n%s", r.code, r.all())
	}
	lift := []string{"hold", "lift", "-sign-key", logRef, "-head-out", heads, "-require-authorization", "-authorizer-pubkey", authPub, "-color", "never"}
	if r := journal(append(lift, common...)...); r.code != 2 || !strings.Contains(r.errOut, "-authorization") {
		t.Fatalf("lift without the authorization: %d\n%s", r.code, r.all())
	}
	if r := journal(append(append(lift, common...), "-authorization", authz)...); r.code != 0 {
		t.Fatalf("lift: %d\n%s", r.code, r.all())
	}
	if r := journal("hold", "list", "-db", db); !strings.Contains(r.out, "no legal hold") {
		t.Fatalf("the hold is still in force: %s", r.all())
	}
	// A PKCS#11 or KMS reference is the only kind of key here: a key file path is refused by the reference parser.
	if r := journal(append(append([]string{"hold", "place", "-matter", "m", "-run", "r2", "-sign-key", filepath.Join(dir, "k.key"), "-head-out", heads}, common...), "-id", "H-2")...); r.code == 0 {
		t.Fatalf("a key file path was accepted as a key reference: %s", r.all())
	}
}
