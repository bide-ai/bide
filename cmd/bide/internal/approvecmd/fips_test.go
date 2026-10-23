package approvecmd

import (
	"crypto/fips140"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
)

// bide keygen -alg ml-dsa-65 (and the hybrid) under Go's FIPS module v1.0.0, which has no ML-DSA, is
// an error that names the cause, not a panic; ed25519 and ecdsa-p256 work under every mode; under
// v1.26.0 and later every scheme works. The assertion is made in every mode.
func TestKeygenSchemesUnderTheFIPSModuleVersion(t *testing.T) {
	noMLDSA := fips140.Version() == "v1.0.0"
	for _, alg := range []audit.Alg{audit.AlgEd25519, audit.AlgECDSAP256, audit.AlgMLDSA65, audit.AlgHybrid} {
		needsMLDSA := alg == audit.AlgMLDSA65 || alg == audit.AlgHybrid
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("newSigner(%s) panicked: %v", alg, r)
				}
			}()
			s, line, err := newSigner(alg)
			switch {
			case needsMLDSA && noMLDSA:
				if err == nil || !strings.Contains(err.Error(), "unavailable in FIPS 140-3") {
					t.Errorf("newSigner(%s) under v1.0.0 = %v, want an error naming the unavailable module", alg, err)
				}
			case err != nil:
				t.Errorf("newSigner(%s) = %v", alg, err)
			case s == nil || line == "":
				t.Errorf("newSigner(%s) returned no key", alg)
			}
		}()
	}
}
