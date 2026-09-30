package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// tsBundle writes a ProofBundle whose signed head carries timestamp ts, and returns its path and
// the key.
func tsBundle(t *testing.T, dir, name string, ts int64) (string, string) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "r", "charge", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, "r", ts)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := audit.ProveRecord(ctx, store, "r", 0, signHead(t, th, audit.Ed25519Signer{Priv: priv}))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	writeJSON(t, path, pb)
	return path, hex.EncodeToString(pub)
}

// Every signed head the CLI reads is held to the timestamp rule: Unix nanoseconds, positive, and
// not later than the clock plus the allowed skew.
func TestCLI_SignedHeadTimestampIsChecked(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	now := time.Now()
	for name, tc := range map[string]struct {
		ts   int64
		code int
	}{
		"an hour ago":                  {now.Add(-time.Hour).UnixNano(), 0},
		"a minute ahead (within skew)": {now.Add(time.Minute).UnixNano(), 0},
		"an hour in the future":        {now.Add(time.Hour).UnixNano(), 1},
		"zero":                         {0, 1},
		"negative":                     {-1, 1},
	} {
		path, pubHex := tsBundle(t, dir, "b.json", tc.ts)
		if code, out := exitCode(t, bin, "verify", "-bundle", path, "-pubkey", pubHex); code != tc.code {
			t.Errorf("a bundle signed %s: exit %d, want %d; output:\n%s", name, code, tc.code, out)
		}
	}
}

// -max-clock-skew sets the allowed skew; a negative one is a usage error.
func TestCLI_MaxClockSkew(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	path, pubHex := tsBundle(t, dir, "b.json", time.Now().Add(time.Hour).UnixNano())
	if code, out := exitCode(t, bin, "verify", "-max-clock-skew", "2h", "-bundle", path, "-pubkey", pubHex); code != 0 {
		t.Errorf("an hour ahead with a two-hour skew: exit %d, output:\n%s", code, out)
	}
	if code, out := exitCode(t, bin, "verify", "-max-clock-skew", "-1ns", "-bundle", path, "-pubkey", pubHex); code != 2 {
		t.Errorf("a negative skew: exit %d, want 2; output:\n%s", code, out)
	}
	path, pubHex = tsBundle(t, dir, "b.json", time.Now().Add(-time.Hour).UnixNano())
	if code, out := exitCode(t, bin, "verify", "-max-clock-skew", "0s", "-bundle", path, "-pubkey", pubHex); code != 0 {
		t.Errorf("an hour ago with no skew: exit %d, output:\n%s", code, out)
	}
}

// verify-run refuses a certificate whose used-policy head is signed before the journal head it was
// projected from.
func TestVerifyRunCLI_UsedPolicyHeadIsNotEarlier(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "r", "charge", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	at := time.Now().Add(-time.Hour)
	th, err := audit.NewTreeHead(ctx, store, "r", at.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	sth := signHead(t, th, audit.Ed25519Signer{Priv: priv})
	for name, tc := range map[string]struct {
		used int64
		code int
	}{
		"same time": {at.UnixNano(), 0},
		"earlier":   {at.Add(-time.Minute).UnixNano(), 1},
	} {
		// CertifyRun refuses an earlier used-policy head, so the producer here re-signs it by hand.
		signer := audit.Ed25519Signer{Priv: priv}
		cert, err := audit.CertifyRun(ctx, store, "r", sth, audit.RunCertSpec{Signer: signer, TimestampNanos: at.UnixNano()})
		if err != nil {
			t.Fatal(err)
		}
		used := cert.UsedPolicyAbsence.TreeHead
		used.TimestampNanos = tc.used
		cert.UsedPolicyAbsence = signHead(t, used, signer)
		path := filepath.Join(dir, "cert.json")
		writeJSON(t, path, cert)
		if code, out := exitCode(t, bin, "verify-run", "-cert", path, "-pubkey", hex.EncodeToString(pub), "-approved", "none"); code != tc.code {
			t.Errorf("used-policy head signed %s: exit %d, want %d; output:\n%s", name, code, tc.code, out)
		}
	}
}

// checkHeadTimes finds a signed head wherever an input carries it.
func TestCheckHeadTimes_FindsEveryHead(t *testing.T) {
	now := time.Now()
	bad := audit.SignedTreeHead{TreeHead: audit.TreeHead{TimestampNanos: 0}}
	good := audit.SignedTreeHead{TreeHead: audit.TreeHead{TimestampNanos: 1}}
	type inner struct{ H audit.SignedTreeHead }
	type hidden struct{ h audit.SignedTreeHead }
	for name, tc := range map[string]struct {
		v   any
		bad bool
	}{
		"direct":                {bad, true},
		"pointer":               {&bad, true},
		"nil pointer":           {(*audit.SignedTreeHead)(nil), false},
		"struct field":          {inner{bad}, true},
		"slice":                 {[]audit.SignedTreeHead{good, bad}, true},
		"array":                 {[2]audit.SignedTreeHead{good, bad}, true},
		"map":                   {map[string]audit.SignedTreeHead{"a": bad}, true},
		"interface":             {[]any{good, bad}, true},
		"all good":              {[]any{good, &good, inner{good}}, false},
		"unexported field":      {hidden{bad}, false},
		"bytes":                 {[]byte{0, 1}, false},
		"a head with good time": {good, false},
	} {
		err := checkHeadTimes(reflect.ValueOf(tc.v), now)
		if (err != nil) != tc.bad {
			t.Errorf("%s: checkHeadTimes = %v, want a failure %v", name, err, tc.bad)
		}
	}
}
