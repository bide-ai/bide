package keyref

import (
	"bytes"
	"strings"
	"testing"
)

func TestParsePKCS11(t *testing.T) {
	r, err := Parse("pkcs11:token=bide%20ops;object=approver-1;id=%01%02%ff?module-path=/usr/lib/softhsm/libsofthsm2.so&pin-source=file:/etc/bide/pin")
	if err != nil {
		t.Fatal(err)
	}
	p := r.PKCS11
	if r.Kind != KindPKCS11 || p == nil || r.KMS != nil {
		t.Fatalf("kind: %+v", r)
	}
	if p.TokenLabel != "bide ops" || p.ObjectLabel != "approver-1" || !bytes.Equal(p.ID, []byte{1, 2, 0xff}) || p.SlotID != nil {
		t.Fatalf("fields: %+v", p)
	}
	if p.ModulePath != "/usr/lib/softhsm/libsofthsm2.so" || p.PINFile != "/etc/bide/pin" || p.PINEnv != "" {
		t.Fatalf("module/pin: %+v", p)
	}
}

func TestParsePKCS11Variants(t *testing.T) {
	for _, tc := range []struct {
		name, ref string
		check     func(t *testing.T, p *PKCS11)
	}{
		{"slot id and id only", "pkcs11:slot-id=7;id=%AB?module-path=/m.so", func(t *testing.T, p *PKCS11) {
			if p.SlotID == nil || *p.SlotID != 7 || p.TokenLabel != "" || p.ObjectLabel != "" || !bytes.Equal(p.ID, []byte{0xab}) {
				t.Fatalf("%+v", p)
			}
		}},
		{"type private", "pkcs11:token=t;object=o;type=private?module-path=/m.so", func(t *testing.T, p *PKCS11) {}},
		{"pin from env", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=env:MY_PIN", func(t *testing.T, p *PKCS11) {
			if p.PINEnv != "MY_PIN" || p.PINFile != "" {
				t.Fatalf("%+v", p)
			}
		}},
		{"pin file as file URL", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=file:///run/secrets/pin", func(t *testing.T, p *PKCS11) {
			if p.PINFile != "/run/secrets/pin" {
				t.Fatalf("%+v", p)
			}
		}},
		{"pin from the terminal", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=prompt", func(t *testing.T, p *PKCS11) {
			if !p.PINPrompt || p.PINFile != "" || p.PINEnv != "" {
				t.Fatalf("%+v", p)
			}
		}},
		{"pin on the token's pad", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=protected", func(t *testing.T, p *PKCS11) {
			if !p.PINProtected || p.PINPrompt || p.PINFile != "" || p.PINEnv != "" {
				t.Fatalf("%+v", p)
			}
		}},
		{"no pin source", "pkcs11:token=t;object=o?module-path=/m.so", func(t *testing.T, p *PKCS11) {
			if p.PINFile != "" || p.PINEnv != "" {
				t.Fatalf("%+v", p)
			}
		}},
		{"percent-encoded module path and label", "pkcs11:token=a%3Bb%3Dc;object=%E2%82%AC?module-path=/opt/my%20hsm/lib.so", func(t *testing.T, p *PKCS11) {
			if p.TokenLabel != "a;b=c" || p.ObjectLabel != "€" || p.ModulePath != "/opt/my hsm/lib.so" {
				t.Fatalf("%+v", p)
			}
		}},
		{"lowercase hex escapes", "pkcs11:token=t;id=%0a%0B?module-path=/m.so", func(t *testing.T, p *PKCS11) {
			if !bytes.Equal(p.ID, []byte{0x0a, 0x0b}) {
				t.Fatalf("%+v", p)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, r.PKCS11)
		})
	}
}

// secret is a value no error may repeat: it stands for a PIN, a token label or a path in a
// reference that is refused.
const secret = "s3cr3tvalue"

func TestParseRefusals(t *testing.T) {
	for _, tc := range []struct{ name, ref, want string }{
		{"empty", "", "scheme"},
		{"no scheme", "token=t;object=o", "scheme"},
		{"other scheme", "https://example.com/key", "scheme"},
		{"uppercase scheme", "PKCS11:token=t;object=o?module-path=/m.so", "scheme"},
		{"pin-value", "pkcs11:token=t;object=o?module-path=/m.so&pin-value=" + secret, "pin-value is refused"},
		{"pin-value first", "pkcs11:token=t;object=o?pin-value=" + secret + "&module-path=/m.so", "pin-value is refused"},
		{"pin-value in the path", "pkcs11:token=t;object=o;pin-value=" + secret + "?module-path=/m.so", "pin-value"},
		{"unknown path attribute", "pkcs11:token=t;object=o;serial=" + secret + "?module-path=/m.so", "serial"},
		{"unknown query attribute", "pkcs11:token=t;object=o?module-path=/m.so&" + "x-evil=" + secret, "not supported"},
		{"module-name", "pkcs11:token=t;object=o?module-name=" + secret, "module-name"},
		{"duplicate attribute", "pkcs11:token=t;token=u;object=o?module-path=/m.so", "token"},
		{"duplicate query attribute", "pkcs11:token=t;object=o?module-path=/m.so&module-path=/n.so", "module-path"},
		{"no module path", "pkcs11:token=t;object=o", "module-path"},
		{"relative module path", "pkcs11:token=t;object=o?module-path=lib/m.so", "module-path"},
		{"empty module path", "pkcs11:token=t;object=o?module-path=", "module-path"},
		{"no token or slot", "pkcs11:object=o?module-path=/m.so", "token"},
		{"token and slot", "pkcs11:token=t;slot-id=1;object=o?module-path=/m.so", "slot-id"},
		{"no object or id", "pkcs11:token=t?module-path=/m.so", "object"},
		{"bad slot", "pkcs11:slot-id=x" + secret + ";object=o?module-path=/m.so", "slot-id"},
		{"negative slot", "pkcs11:slot-id=-1;object=o?module-path=/m.so", "slot-id"},
		{"public key type", "pkcs11:token=t;object=o;type=public?module-path=/m.so", "type"},
		{"bad escape", "pkcs11:token=t%zz" + secret + ";object=o?module-path=/m.so", "percent"},
		{"truncated escape", "pkcs11:token=t%4;object=o?module-path=/m.so", "percent"},
		{"raw space", "pkcs11:token=t " + secret + ";object=o?module-path=/m.so", "percent-encoded"},
		{"raw newline", "pkcs11:token=t\n;object=o?module-path=/m.so", "percent-encoded"},
		{"control character in a label", "pkcs11:token=t%00" + secret + ";object=o?module-path=/m.so", "control"},
		{"bidi override in a label", "pkcs11:token=t%E2%80%AE" + secret + ";object=o?module-path=/m.so", "control"},
		{"invalid utf-8", "pkcs11:token=%ff" + secret + ";object=o?module-path=/m.so", "UTF-8"},
		{"empty id", "pkcs11:token=t;id=?module-path=/m.so", "id is empty"},
		{"fragment", "pkcs11:token=t;object=o?module-path=/m.so#" + secret, "fragment"},
		{"attribute without value", "pkcs11:token=t;object;" + secret + "?module-path=/m.so", "malformed"},
		{"empty attribute", "pkcs11:token=t;;object=o?module-path=/m.so", "malformed"},
		{"pin-source http", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=http://" + secret, "pin-source"},
		{"pin-source relative file", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=file:pin", "pin-source"},
		{"pin-source remote file host", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=file://" + secret + "/pin", "pin-source"},
		{"pin-source env bad name", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=env:1" + secret, "pin-source"},
		{"pin-source prompt with a value", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=prompt:" + secret, "pin-source"},
		{"pin-source protected with a value", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=protected:" + secret, "pin-source"},
		{"pin-source prompt uppercase", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=PROMPT", "pin-source"},
		{"pin-source other", "pkcs11:token=t;object=o?module-path=/m.so&pin-source=" + secret, "pin-source"},
		{"too long", "pkcs11:token=" + strings.Repeat("a", 3000) + ";object=o?module-path=/m.so", "long"},
		{"kms with query", "awskms:arn:aws:kms:us-east-1:123456789012:key/abc?x=" + secret, "no query or fragment"},
		{"kms not an arn", "awskms:alias/" + secret, "ARN"},
		{"kms key id only", "awskms:1234abcd-12ab-34cd-56ef-1234567890ab", "ARN"},
		{"kms wrong service", "awskms:arn:aws:s3:us-east-1:123456789012:key/abc", "ARN"},
		{"kms bad partition", "awskms:arn:evil:kms:us-east-1:123456789012:key/abc", "ARN"},
		{"kms short account", "awskms:arn:aws:kms:us-east-1:1234:key/abc", "ARN"},
		{"kms no region", "awskms:arn:aws:kms::123456789012:key/abc", "ARN"},
		{"kms resource not key or alias", "awskms:arn:aws:kms:us-east-1:123456789012:grant/abc", "ARN"},
		{"kms percent", "awskms:arn:aws:kms:us-east-1:123456789012:key/a%2Fb" + secret, "ARN"},
		{"kms whitespace", "awskms:arn:aws:kms:us-east-1:123456789012:key/a b" + secret, "ARN"},
		{"kms empty", "awskms:", "ARN"},
		{"kms standard partition, gov region", "awskms:arn:aws:kms:us-gov-west-1:123456789012:key/abc", "ARN"},
		{"kms china partition, standard region", "awskms:arn:aws-cn:kms:us-east-1:123456789012:key/abc", "ARN"},
		{"kms gov partition, standard region", "awskms:arn:aws-us-gov:kms:us-east-1:123456789012:key/abc", "ARN"},
		{"kms standard partition, iso region", "awskms:arn:aws:kms:us-isob-east-1:123456789012:key/abc", "ARN"},
		{"kms alias with an empty segment", "awskms:arn:aws:kms:us-east-1:123456789012:alias//a", "ARN"},
		{"kms alias ending in a slash", "awskms:arn:aws:kms:us-east-1:123456789012:alias/a/", "ARN"},
		{"kms alias with nothing", "awskms:arn:aws:kms:us-east-1:123456789012:alias/", "ARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse(tc.ref)
			if err == nil {
				t.Fatalf("Parse(%q) = %+v, want an error", tc.ref, r)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not mention %q", msg, tc.want)
			}
			if strings.Contains(msg, secret) {
				t.Errorf("error %q repeats a value of the reference", msg)
			}
		})
	}
}

func TestParseKMS(t *testing.T) {
	for _, tc := range []struct{ ref, region, arn string }{
		{"awskms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab", "eu-west-1", "arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"},
		{"awskms:arn:aws:kms:us-east-1:123456789012:key/mrk-0123456789abcdef0123456789abcdef", "us-east-1", "arn:aws:kms:us-east-1:123456789012:key/mrk-0123456789abcdef0123456789abcdef"},
		{"awskms:arn:aws-us-gov:kms:us-gov-west-1:123456789012:alias/bide/approver_1", "us-gov-west-1", "arn:aws-us-gov:kms:us-gov-west-1:123456789012:alias/bide/approver_1"},
		{"awskms:arn:aws-cn:kms:cn-north-1:123456789012:key/abc-def", "cn-north-1", "arn:aws-cn:kms:cn-north-1:123456789012:key/abc-def"},
		{"awskms:arn:aws-iso:kms:us-iso-east-1:123456789012:key/abc", "us-iso-east-1", "arn:aws-iso:kms:us-iso-east-1:123456789012:key/abc"},
		{"awskms:arn:aws-iso-b:kms:us-isob-east-1:123456789012:key/abc", "us-isob-east-1", "arn:aws-iso-b:kms:us-isob-east-1:123456789012:key/abc"},
		{"awskms:arn:aws-iso-e:kms:eu-isoe-west-1:123456789012:key/abc", "eu-isoe-west-1", "arn:aws-iso-e:kms:eu-isoe-west-1:123456789012:key/abc"},
		{"awskms:arn:aws-iso-f:kms:us-isof-south-1:123456789012:key/abc", "us-isof-south-1", "arn:aws-iso-f:kms:us-isof-south-1:123456789012:key/abc"},
		{"awskms:arn:aws-eusc:kms:eusc-de-east-1:123456789012:key/abc", "eusc-de-east-1", "arn:aws-eusc:kms:eusc-de-east-1:123456789012:key/abc"},
		{"awskms:arn:aws:kms:ap-southeast-2:123456789012:alias/a/b_c-d", "ap-southeast-2", "arn:aws:kms:ap-southeast-2:123456789012:alias/a/b_c-d"},
	} {
		r, err := Parse(tc.ref)
		if err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
		if r.Kind != KindAWSKMS || r.KMS == nil || r.PKCS11 != nil || r.KMS.ARN != tc.arn || r.KMS.Region != tc.region {
			t.Fatalf("%s: %+v", tc.ref, r)
		}
	}
}

func TestRefString(t *testing.T) {
	// String is for display: it names the key and never a PIN source's content.
	r, err := Parse("pkcs11:token=bide;object=ops?module-path=/m.so&pin-source=file:/run/pin")
	if err != nil {
		t.Fatal(err)
	}
	if s := r.String(); !strings.Contains(s, "token=bide") || !strings.Contains(s, "object=ops") || strings.Contains(s, "/run/pin") {
		t.Fatalf("String = %q", s)
	}
	k, _ := Parse("awskms:arn:aws:kms:us-east-1:123456789012:key/abc")
	if k.String() != "awskms:arn:aws:kms:us-east-1:123456789012:key/abc" {
		t.Fatalf("String = %q", k.String())
	}
}

func TestParseGCPKMS(t *testing.T) {
	const ok = "projects/bank-prod/locations/europe-west2/keyRings/bide/cryptoKeys/approver/cryptoKeyVersions/3"
	r, err := Parse("gcpkms:" + ok)
	if err != nil || r.Kind != KindGCPKMS || r.GCP == nil || r.GCP.Version != ok || r.PKCS11 != nil || r.KMS != nil || r.String() != "gcpkms:"+ok {
		t.Fatalf("%+v %v", r, err)
	}
	for name, ref := range map[string]string{
		"empty":                "gcpkms:",
		"no version":           "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/k",
		"latest":               "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/latest",
		"primary":              "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/primary",
		"version zero":         "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/0",
		"leading zero":         "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/01",
		"signed version":       "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/+1",
		"query":                "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1?x=" + secret,
		"fragment":             "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1#" + secret,
		"trailing slash":       "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1/",
		"empty segment":        "gcpkms:projects//locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		"escape":               "gcpkms:projects/p%2e/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		"space":                "gcpkms:projects/p /locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		"dot dot":              "gcpkms:projects/p/locations/l/keyRings/r/cryptoKeys/../cryptoKeyVersions/1",
		"other resource":       "gcpkms:projects/p/locations/l/keyRings/r/importJobs/j/cryptoKeyVersions/1",
		"uppercase scheme":     "GCPKMS:projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		"a secret in the path": "gcpkms:projects/" + secret + "/locations/l/keyRings/r/cryptoKeys/k",
	} {
		if r, err := Parse(ref); err == nil {
			t.Errorf("%s: %q parsed as %+v", name, ref, r)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error repeats a value: %v", name, err)
		}
	}
}

func TestParseAzureKV(t *testing.T) {
	const id = "https://bank-prod.vault.azure.net/keys/approver-1/0123456789abcdef0123456789ABCDEF"
	r, err := Parse("azurekv:" + id)
	if err != nil || r.Kind != KindAzureKV || r.Azure == nil || r.Azure.KeyID != id || r.GCP != nil || r.String() != "azurekv:"+id {
		t.Fatalf("%+v %v", r, err)
	}
	for _, h := range []string{"hsm1.managedhsm.azure.net", "v.vault.azure.cn", "v.managedhsm.azure.cn", "v.vault.usgovcloudapi.net", "v.managedhsm.usgovcloudapi.net"} {
		if _, err := Parse("azurekv:https://" + h + "/keys/k/0123456789abcdef0123456789abcdef"); err != nil {
			t.Errorf("%s: %v", h, err)
		}
	}
	const v = "0123456789abcdef0123456789abcdef"
	for name, ref := range map[string]string{
		"empty":            "azurekv:",
		"no version":       "azurekv:https://v.vault.azure.net/keys/k",
		"empty version":    "azurekv:https://v.vault.azure.net/keys/k/",
		"short version":    "azurekv:https://v.vault.azure.net/keys/k/0123",
		"non-hex version":  "azurekv:https://v.vault.azure.net/keys/k/0123456789abcdef0123456789abcdeg",
		"http":             "azurekv:http://v.vault.azure.net/keys/k/" + v,
		"other host":       "azurekv:https://evil.example.com/keys/k/" + v,
		"lookalike host":   "azurekv:https://vault.azure.net.evil.com/keys/k/" + v,
		"bare suffix":      "azurekv:https://.vault.azure.net/keys/k/" + v,
		"port":             "azurekv:https://v.vault.azure.net:8443/keys/k/" + v,
		"empty port":       "azurekv:https://v.vault.azure.net:/keys/k/" + v,
		"credentials":      "azurekv:https://" + secret + "@v.vault.azure.net/keys/k/" + v,
		"query":            "azurekv:https://v.vault.azure.net/keys/k/" + v + "?x=" + secret,
		"fragment":         "azurekv:https://v.vault.azure.net/keys/k/" + v + "#" + secret,
		"empty query":      "azurekv:https://v.vault.azure.net/keys/k/" + v + "?",
		"upper-case host":  "azurekv:https://V.vault.azure.net/keys/k/" + v,
		"secrets path":     "azurekv:https://v.vault.azure.net/secrets/k/" + v,
		"extra segment":    "azurekv:https://v.vault.azure.net/keys/k/" + v + "/x",
		"escaped name":     "azurekv:https://v.vault.azure.net/keys/k%2e/" + v,
		"bad name":         "azurekv:https://v.vault.azure.net/keys/" + secret + "_x/" + v,
		"no scheme":        "azurekv:v.vault.azure.net/keys/k/" + v,
		"whitespace":       "azurekv:https://v.vault.azure.net/keys/k /" + v,
		"uppercase scheme": "AZUREKV:https://v.vault.azure.net/keys/k/" + v,
	} {
		if r, err := Parse(ref); err == nil {
			t.Errorf("%s: %q parsed as %+v", name, ref, r)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error repeats a value: %v", name, err)
		}
	}
}

// A base identifier is refused for what it is, not only as a malformed one: it would follow rotation.
func TestAzureKVBaseIdentifierIsNamed(t *testing.T) {
	for _, ref := range []string{"azurekv:https://v.vault.azure.net/keys/k", "azurekv:https://v.vault.azure.net/keys/k/"} {
		if _, err := Parse(ref); err == nil || !strings.Contains(err.Error(), "pin the version") {
			t.Errorf("%s: %v", ref, err)
		}
	}
}
