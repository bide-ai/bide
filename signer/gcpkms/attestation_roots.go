package gcpkms

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
)

// The default trust anchors for HSM attestation. Both are public certificates; their provenance is
// in the key custody guide and in testdata/attestation/README.md, and their SHA-256 fingerprints are
// pinned below and by a test, so a change is deliberate.
const (
	// googleOwnerRootPEM is Google's Cloud HSM owner root, "Hawksbill Root v1 prod", as in
	// GoogleCloudPlatform/python-docs-samples kms/attestations/verify_attestation_chains.py and at
	// https://www.gstatic.com/cloudhsm/roots/global_1498867200.pem. It expires 2030-01-01.
	googleOwnerRootPEM = `-----BEGIN CERTIFICATE-----
MIIDjTCCAnWgAwIBAgIBAzANBgkqhkiG9w0BAQsFADBoMQswCQYDVQQGEwJVUzEL
MAkGA1UECAwCQ0ExFjAUBgNVBAcMDU1vdW50YWluIFZpZXcxEzARBgNVBAoMCkdv
b2dsZSBJbmMxHzAdBgNVBAMMFkhhd2tzYmlsbCBSb290IHYxIHByb2QwHhcNMTcw
NzAxMDAwMDAwWhcNMzAwMTAxMDAwMDAwWjBoMQswCQYDVQQGEwJVUzELMAkGA1UE
CAwCQ0ExFjAUBgNVBAcMDU1vdW50YWluIFZpZXcxEzARBgNVBAoMCkdvb2dsZSBJ
bmMxHzAdBgNVBAMMFkhhd2tzYmlsbCBSb290IHYxIHByb2QwggEiMA0GCSqGSIb3
DQEBAQUAA4IBDwAwggEKAoIBAQCsLqhiiSGgcJLfsI7Dk00mONulol9rHm2obCyD
1lua+AKg+LAW+1zauZu5i028FSbgDk8vtSBDHDF+XsFnqTbIGV7Ctai2lnaQe1UV
TVMWEPBi1diYGceeDrJpJqPz2aXTcIghrGISeyq+IC4z25uQp7G/D8AResKYqYxN
NqcfZlMIk0s6Eh4aPyvCXYtLl9QXD0GDJ6nz4NmC+Fw31B5d5Kg9WXxDZOYC1zU5
9JXbdxxzeC/EJo1k1AHghto/J8edvTIl5NQ0ahOHKoUZzhhDRsVBioFmymVuwaHO
cXTUsHe3NTkNyeLIfoFpsQQ4XcH9kjO67YXTkdCWeNYw/FYZAgMBAAGjQjBAMA8G
A1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgGGMB0GA1UdDgQWBBQx6FLf4Un4
Ent8budOkXqXdbyorjANBgkqhkiG9w0BAQsFAAOCAQEAjxKOjnr7WYKoD+a+uAld
F8iOwTrHpFLUDS6sqFyx9FLut8Qlmioy/JE9uima7cjeH3U5VBbRcnTglaDiQTac
+JXCIRApEl9N0bDhoVvFeTzRI8nJdMJCWPobNXV3MHpYsgfgzewh4lFUWQghvscF
325VgSEN0a1hgXcnPr05gd+9kTI9zF3r3vyncyYvzYincGX0NQaz1gJW4brm1W+w
TbWVy8Y0o6c1eZm7v8sHoNSg3vIs6JsnQ8bAXK5i2qO/AXZQu25wH1aPQct8QdGw
x2JBsjEjmWpHuBDAXPCesD5cu9UzzDgcpdwmi7Xidl74kj3f/HgrOeimRdOb8lG5
/A==
-----END CERTIFICATE-----
`
	googleOwnerRootSHA256 = "46b5fd351d56a0721ca0afcd1731c0f7b74e3941eb818bfd0ec36e29df0de095"

	// marvellRootPEM is the HSM manufacturer's root, "localca.liquidsecurity.cavium.com", the
	// liquid_security_certificate.crt in Marvell's liquid_security_certificate.zip. It expired on
	// 2025-11-16; validity periods are not enforced (see Config.AttestationRoots).
	marvellRootPEM = `-----BEGIN CERTIFICATE-----
MIIDoDCCAogCCQDA6q30NN7cFzANBgkqhkiG9w0BAQsFADCBkTELMAkGA1UEBhMC
VVMxEzARBgNVBAgMCkNhbGlmb3JuaWExETAPBgNVBAcMCFNhbiBKb3NlMRUwEwYD
VQQKDAxDYXZpdW0sIEluYy4xFzAVBgNVBAsMDkxpcXVpZFNlY3VyaXR5MSowKAYD
VQQDDCFsb2NhbGNhLmxpcXVpZHNlY3VyaXR5LmNhdml1bS5jb20wHhcNMTUxMTE5
MTM1NTI1WhcNMjUxMTE2MTM1NTI1WjCBkTELMAkGA1UEBhMCVVMxEzARBgNVBAgM
CkNhbGlmb3JuaWExETAPBgNVBAcMCFNhbiBKb3NlMRUwEwYDVQQKDAxDYXZpdW0s
IEluYy4xFzAVBgNVBAsMDkxpcXVpZFNlY3VyaXR5MSowKAYDVQQDDCFsb2NhbGNh
LmxpcXVpZHNlY3VyaXR5LmNhdml1bS5jb20wggEiMA0GCSqGSIb3DQEBAQUAA4IB
DwAwggEKAoIBAQDckvqQM4cvZjdyqOLGMTjKJwvfxJOhVqw6pojgUMz10VU7z3Ct
JrwHcESwEDUxUkMxzof55kForURLaVVCjedYauEisnZwwSWkAemp9GREm8iX6BXt
oZ8VDWoO2H0AJiHCM62qJeZVXhm8A/zWG0PyLrCINH0yz9ah6BcwdsZGLvQvkpUN
JhwVMrb9nI9BlRmTWhoot1YSTf7jfibEkc/pN+0Ez30RFaL3MhyIaNJS22+10tny
4sOUTsPEtXKah5mPlHpnrGcB18z5Yxgr0vDNYx+FCPGo95XGrq9NYfNMlwsSeFSr
8D1VQ7HZmipeTB1hQTUQw/K/Rmtw5NiljkYTAgMBAAEwDQYJKoZIhvcNAQELBQAD
ggEBAJjqbFaa3FOXEXcXPX2lCHdcyl8TwOR9f3Rq87MEfb3oeK9FarNkUCdvuGs3
OkAWoFib/9l2F7ZgaNlJqVrwBaOvYuGguQoUpDybqttYUJVLcu9vA9eZA+UCJdhd
P7fCyGMO+G96cnG3GTS1/SrIDU+YCnVElQ0P/73/de+ImoeMkwcqiUi2lsf3vGGR
YXMt/DxUwjXwjIpWCs+37cwbNHAv0VKDOR/jmNf5EZf+sy4x2rJZ1NS6eDZ9RBug
CLaN6ntybV4YlE7jDI9XIOm/tPJULZGLpLolngWVB6qtzn1RjBw1HIqpoXg+9s1g
pLFFinSrEL1fkQR0YZQrJckktPs=
-----END CERTIFICATE-----
`
	marvellRootSHA256 = "975757f0d76640e03d14760f8fc9e3a55826fa7807b2c392f7801a95bd69cc28"
)

// AttestationRoots are the trust anchors for HSM attestation: the HSM manufacturer's root and the
// owner (Google) root, as certificates the caller has obtained and checked out of band. Both are
// required. They replace the built-in defaults as a pair.
type AttestationRoots struct {
	// Manufacturer is the HSM manufacturer's self-signed root (Marvell's "localca.liquidsecurity.cavium.com").
	Manufacturer *x509.Certificate
	// Owner is the root that Google signs the card and partition certificates with ("Hawksbill Root v1 prod").
	Owner *x509.Certificate
}

// defaultAttestationRoots parses the built-in roots and checks their pinned fingerprints.
func defaultAttestationRoots() (*AttestationRoots, error) {
	owner, err := pinnedRoot(googleOwnerRootPEM, googleOwnerRootSHA256)
	if err != nil {
		return nil, fmt.Errorf("gcpkms: built-in owner root: %w", err)
	}
	mfr, err := pinnedRoot(marvellRootPEM, marvellRootSHA256)
	if err != nil {
		return nil, fmt.Errorf("gcpkms: built-in manufacturer root: %w", err)
	}
	return &AttestationRoots{Manufacturer: mfr, Owner: owner}, nil
}

func pinnedRoot(p, wantSHA256 string) (*x509.Certificate, error) {
	blk, _ := pem.Decode([]byte(p))
	if blk == nil {
		return nil, fmt.Errorf("not PEM")
	}
	if sum := sha256.Sum256(blk.Bytes); hex.EncodeToString(sum[:]) != wantSHA256 {
		return nil, fmt.Errorf("fingerprint %x is not the pinned one", sum)
	}
	return x509.ParseCertificate(blk.Bytes)
}
