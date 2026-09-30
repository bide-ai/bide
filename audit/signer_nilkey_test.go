package audit

import "testing"

// A signer or verifier without a key is caller input like any other: it signs and verifies
// nothing, and returns an error rather than panicking (a library panic on caller input would take
// down a process that only asked for a signature).
func TestSignersWithoutAKeyDoNotPanic(t *testing.T) {
	msg := []byte("m")
	for name, s := range map[string]Signer{"ed25519": Ed25519Signer{}, "ml-dsa-65": MLDSASigner{}, "hybrid": HybridSigner{}} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: Sign with no key panicked: %v", name, r)
				}
			}()
			if sig, err := s.Sign(msg); err == nil {
				t.Errorf("%s: Sign with no key returned a %d-byte signature and no error", name, len(sig))
			}
		}()
	}
	for name, v := range map[string]Verifier{"ed25519": Ed25519Verifier{}, "ml-dsa-65": MLDSAVerifier{}, "hybrid": HybridVerifier{}} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: Verify with no key panicked: %v", name, r)
				}
			}()
			if v.Verify(msg, append([]byte{0, 0, 0, 64}, make([]byte, 64+3309)...)) {
				t.Errorf("%s: Verify with no key verified", name)
			}
		}()
	}
}
