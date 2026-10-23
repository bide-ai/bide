// Package fipsmode holds tests that pin what bide does under Go's FIPS 140-3 mode: that the CI job
// for it really ran in the mode it names, that the approved signature schemes work under
// GODEBUG=fips140=only, and that ML-DSA and hybrid keys, which Go's module v1.0.0 lacks, are refused
// with an error and never a panic. It makes no claim of FIPS validation.
package fipsmode

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/fips140"
	"crypto/mldsa"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"os"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
)

// buildSetting is the GOFIPS140 value the test binary was built with.
func buildSetting() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "GOFIPS140" {
				return s.Value
			}
		}
	}
	return ""
}

// The FIPS CI job sets BIDE_EXPECT_FIPS to "<GOFIPS140 value>" and runs with GODEBUG=fips140=only;
// this proves the binary was built and is running that way, so a job whose environment silently
// lost a setting fails here instead of passing as an ordinary run. Unset, it only logs the mode.
func TestRunsInTheModeTheJobNames(t *testing.T) {
	t.Logf("fips140.Enabled=%v Enforced=%v Version=%s GOFIPS140 build setting=%q", fips140.Enabled(), fips140.Enforced(), fips140.Version(), buildSetting())
	want := os.Getenv("BIDE_EXPECT_FIPS")
	if want == "" {
		return
	}
	if !fips140.Enabled() || !fips140.Enforced() {
		t.Fatalf("BIDE_EXPECT_FIPS=%q but fips140.Enabled=%v Enforced=%v: GODEBUG=fips140=only is not in effect", want, fips140.Enabled(), fips140.Enforced())
	}
	if want == "off" {
		if v := fips140.Version(); v != "latest" {
			t.Fatalf("GOFIPS140=off should run the in-tree module (Version latest), got %q", v)
		}
		return
	}
	if v := fips140.Version(); v != want {
		t.Fatalf("fips140.Version() = %q, want %q", v, want)
	}
	if !strings.HasPrefix(buildSetting(), want) {
		t.Fatalf("GOFIPS140 build setting = %q, want it to start with %q", buildSetting(), want)
	}
}

// Ed25519 and ECDSA P-256 sign and verify under every mode, including GODEBUG=fips140=only: a tree
// head (what an anchor holds) signed and verified, a tampered one refused, and the verifier rebuilt
// from the public key alone as an offline auditor does.
func TestApprovedSchemesSignAndVerifyUnderFIPSOnly(t *testing.T) {
	ed, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = ed
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []audit.Signer{audit.Ed25519Signer{Priv: edPriv}, audit.ECDSAP256Signer{Priv: p256}} {
		t.Run(string(s.Alg()), func(t *testing.T) {
			th := audit.TreeHead{Kind: audit.TreeJournal, RunID: "run-fips", Size: 3, Root: make([]byte, 32), TimestampNanos: time.Now().UnixNano()}
			sth, err := audit.SignTreeHead(th, s)
			if err != nil {
				t.Fatalf("SignTreeHead: %v", err)
			}
			v, err := audit.NewVerifier(s.Alg(), s.PublicKey())
			if err != nil {
				t.Fatalf("NewVerifier from the public key: %v", err)
			}
			if err := sth.Verify(v); err != nil {
				t.Fatalf("verify: %v", err)
			}
			bad := sth
			bad.Size++
			if bad.Verify(v) == nil {
				t.Fatal("a tampered tree head verified")
			}
			sig, err := s.Sign([]byte("approval"))
			if err != nil || !v.Verify([]byte("approval"), sig) || v.Verify([]byte("other"), sig) {
				t.Fatalf("Sign/Verify round trip: err %v", err)
			}
		})
	}
}

// Under Go's FIPS module v1.0.0 (no ML-DSA), selecting ML-DSA or hybrid is an error that names the
// cause, never a panic: key generation, loading a public key, and signing with a signer that holds no
// key. Under v1.26.0 and later the same calls work. Either way the assertion is made, none skipped.
func TestMLDSAAndHybridUnderTheModuleVersion(t *testing.T) {
	noMLDSA := fips140.Version() == "v1.0.0"
	k, genErr := mldsa.GenerateKey(mldsa.MLDSA65())
	if noMLDSA {
		if genErr == nil || !strings.Contains(genErr.Error(), "unavailable in FIPS 140-3") {
			t.Fatalf("mldsa.GenerateKey under v1.0.0 = %v, want the unavailable error", genErr)
		}
	} else if genErr != nil {
		t.Fatalf("mldsa.GenerateKey = %v, want a key", genErr)
	}

	// A well-formed 1952-byte ML-DSA-65 public key, and a hybrid key around it.
	mlPub := make([]byte, mldsa.MLDSA65PublicKeySize)
	if k != nil {
		mlPub = k.PublicKey().Bytes()
	}
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	hybrid := binary.BigEndian.AppendUint32(nil, uint32(len(edPub)))
	hybrid = append(append(hybrid, edPub...), mlPub...)

	for _, c := range []struct {
		alg audit.Alg
		pub []byte
	}{{audit.AlgMLDSA65, mlPub}, {audit.AlgHybrid, hybrid}} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("NewVerifier(%s) panicked: %v", c.alg, r)
				}
			}()
			_, err := audit.NewVerifier(c.alg, c.pub)
			if noMLDSA {
				if !errors.Is(err, audit.ErrMalformed) || !strings.Contains(err.Error(), "unavailable in FIPS 140-3") {
					t.Errorf("NewVerifier(%s) under v1.0.0 = %v, want an error naming the unavailable module", c.alg, err)
				}
			} else if err != nil {
				t.Errorf("NewVerifier(%s) = %v", c.alg, err)
			}
		}()
	}

	// Signers that hold no ML-DSA key refuse to sign, with an error, in every mode.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("signing without a key panicked: %v", r)
			}
		}()
		if _, err := (audit.MLDSASigner{}).Sign([]byte("m")); err == nil {
			t.Error("MLDSASigner with no key signed")
		}
		if _, err := (audit.HybridSigner{}).Sign([]byte("m")); err == nil {
			t.Error("HybridSigner with no keys signed")
		}
	}()
}
