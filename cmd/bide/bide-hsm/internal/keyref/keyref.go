// Package keyref parses the references bide-hsm takes for an approver's key: an RFC 7512 PKCS#11
// URI for a key in an HSM or on a token, and an AWS KMS key ARN for a key in a KMS.
//
// # PKCS#11 URIs
//
//	pkcs11:token=<label>;object=<label>;id=<%xx...>?module-path=<abs path>&pin-source=<source>
//
// The path attributes select the token and the key: token (the token's label) or slot-id (its
// slot number), exactly one of them; and object (the key's CKA_LABEL), id (its CKA_ID, as
// percent-encoded bytes), or both, at least one; type may be given, and must be private. The query
// holds module-path, the absolute path of the vendor's PKCS#11 library (required), and
// pin-source, where the user PIN is read from: file:/absolute/path, or env:NAME or prompt (extensions
// of RFC 7512, which names only URIs here; prompt asks on the controlling terminal with echo off; protected leaves the PIN to the token's own
// PIN pad, and is refused for a token without one). Without pin-source the PIN is read from the environment
// variable BIDE_HSM_PIN.
//
// The parser is strict. It refuses every attribute it does not act on, rather than ignoring it,
// because an ignored attribute (serial, manufacturer) would let a reference select a key more
// loosely than it reads. It refuses pin-value, which would put the PIN in the process list and the
// shell history; it refuses a repeated attribute, a malformed percent escape, a character that must
// be percent-encoded, and a label that holds a control, bidirectional or invisible character. An
// error from Parse never repeats a value from the reference, so a PIN typed into the wrong attribute is
// not echoed by the parser.
//
// # KMS references
//
//	awskms:arn:aws:kms:<region>:<account>:key/<id>
//	awskms:arn:aws:kms:<region>:<account>:alias/<name>
//
// A full ARN, so the reference names the region and the account and does not depend on the
// environment for them. Credentials come from the AWS SDK's default chain.
//
//	gcpkms:projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>/cryptoKeyVersions/<n>
//
// A full CryptoKeyVersion resource name with a numeric version: no alias, no "latest", so the
// reference names one key version and signing uses exactly that. Credentials come from Google's
// application default credentials.
package keyref

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bide-ai/bide/internal/approveline"
)

// Kind says which kind of key a reference names.
type Kind int

// The kinds of key a reference can name.
const (
	KindPKCS11 Kind = iota + 1
	KindAWSKMS
	KindGCPKMS
)

// Ref is a parsed key reference. Exactly one of PKCS11 and KMS is set, as Kind says.
type Ref struct {
	Kind   Kind
	PKCS11 *PKCS11
	KMS    *KMS
	GCP    *GCPKMS
}

// PKCS11 is a parsed pkcs11: URI.
type PKCS11 struct {
	ModulePath   string // module-path: the vendor library
	TokenLabel   string // token; empty when SlotID is set
	SlotID       *uint  // slot-id; nil when TokenLabel is set
	ObjectLabel  string // object: the key's CKA_LABEL; may be empty when ID is set
	ID           []byte // id: the key's CKA_ID; may be empty when ObjectLabel is set
	PINFile      string // pin-source=file:...; at most one of PINFile, PINEnv and PINPrompt
	PINEnv       string // pin-source=env:...
	PINPrompt    bool   // pin-source=prompt: ask on the controlling terminal
	PINProtected bool   // pin-source=protected: the token collects the PIN on its own PIN pad
}

// GCPKMS is a parsed gcpkms: reference.
type GCPKMS struct {
	Version string // the full CryptoKeyVersion resource name
}

// KMS is a parsed awskms: reference.
type KMS struct {
	ARN    string // the key or alias ARN
	Region string // the ARN's region
}

// MaxLen bounds a reference.
const MaxLen = 2048

// DefaultPINEnv is the environment variable a PKCS#11 PIN is read from when the URI has no
// pin-source.
const DefaultPINEnv = "BIDE_HSM_PIN"

// known are the attribute names RFC 7512 defines. An error names an attribute only if it is one of
// these, so that it never repeats text of the reference that is not a defined name.
var known = map[string]bool{
	"token": true, "manufacturer": true, "serial": true, "model": true, "library-manufacturer": true,
	"library-description": true, "library-version": true, "slot-description": true,
	"slot-manufacturer": true, "slot-id": true, "id": true, "object": true, "type": true,
	"module-name": true, "module-path": true, "pin-source": true, "pin-value": true,
}

var pathAttrs = map[string]bool{"token": true, "slot-id": true, "object": true, "id": true, "type": true}
var queryAttrs = map[string]bool{"module-path": true, "pin-source": true}

// gcpVersion is the shape of a CryptoKeyVersion resource name, as signer/gcpkms accepts it: the version
// is the number Cloud KMS assigns, so an alias or "latest" is not a version.
var gcpVersion = regexp.MustCompile(`^projects/[A-Za-z0-9_:.-]+/locations/[A-Za-z0-9_-]+/keyRings/[A-Za-z0-9_-]+/cryptoKeys/[A-Za-z0-9_-]+/cryptoKeyVersions/[1-9][0-9]*$`)

// kmsARN is a KMS key or alias ARN. An alias is alias/ and a name of letters, digits, / _ and -,
// without an empty path segment.
var kmsARN = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov|aws-iso|aws-iso-b|aws-iso-e|aws-iso-f|aws-eusc):kms:([a-z]{2,5}(?:-[a-z]+)+-[0-9]+):([0-9]{12}):(key/[A-Za-z0-9-]{1,128}|alias/[A-Za-z0-9_-]{1,256}(?:/[A-Za-z0-9_-]{1,256})*)$`)

// partitionRegion is the prefix every region of a partition starts with; the standard partition's
// regions start with none of the others'.
var partitionRegion = map[string]string{
	"aws-cn": "cn-", "aws-us-gov": "us-gov-", "aws-iso": "us-iso-", "aws-iso-b": "us-isob-",
	"aws-iso-e": "eu-isoe-", "aws-iso-f": "us-isof-", "aws-eusc": "eusc-",
}

// regionInPartition reports whether region is a region of the partition: a mismatched ARN names no
// key, and AWS would refuse it later, far from the reference.
func regionInPartition(partition, region string) bool {
	if prefix, ok := partitionRegion[partition]; ok {
		return strings.HasPrefix(region, prefix)
	}
	for _, other := range partitionRegion {
		if strings.HasPrefix(region, other) {
			return false
		}
	}
	return true
}

// Parse reads a key reference. See the package comment for the forms.
func Parse(s string) (Ref, error) {
	if len(s) > MaxLen {
		return Ref{}, errors.New("key reference is too long")
	}
	switch {
	case strings.HasPrefix(s, "pkcs11:"):
		p, err := parsePKCS11(strings.TrimPrefix(s, "pkcs11:"))
		if err != nil {
			return Ref{}, fmt.Errorf("pkcs11 URI: %w", err)
		}
		return Ref{Kind: KindPKCS11, PKCS11: p}, nil
	case strings.HasPrefix(s, "awskms:"):
		k, err := parseKMS(strings.TrimPrefix(s, "awskms:"))
		if err != nil {
			return Ref{}, err
		}
		return Ref{Kind: KindAWSKMS, KMS: k}, nil
	case strings.HasPrefix(s, "gcpkms:"):
		v := strings.TrimPrefix(s, "gcpkms:")
		if !gcpVersion.MatchString(v) {
			return Ref{}, errors.New("gcpkms: not a CryptoKeyVersion resource name: want gcpkms:projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>/cryptoKeyVersions/<n> with a numeric version (no alias, no latest, no query)")
		}
		return Ref{Kind: KindGCPKMS, GCP: &GCPKMS{Version: v}}, nil
	}
	return Ref{}, errors.New("key reference has no scheme bide-hsm knows: want a pkcs11: URI (RFC 7512), awskms:<key ARN> or gcpkms:<key version>")
}

func parseKMS(rest string) (*KMS, error) {
	if strings.ContainsAny(rest, "?#") {
		return nil, errors.New("awskms: a reference is the key ARN alone, with no query or fragment")
	}
	m := kmsARN.FindStringSubmatch(rest)
	if m == nil {
		return nil, errors.New("awskms: not a KMS key or alias ARN: want awskms:arn:aws:kms:<region>:<12-digit account>:key/<id> (or alias/<name>)")
	}
	if !regionInPartition(m[1], m[2]) {
		return nil, errors.New("awskms: the region is not a region of the ARN's partition (ARN)")
	}
	return &KMS{ARN: rest, Region: m[2]}, nil
}

func parsePKCS11(rest string) (*PKCS11, error) {
	if strings.Contains(rest, "#") {
		return nil, errors.New("a fragment is not part of a key reference")
	}
	path, query, _ := strings.Cut(rest, "?")
	if strings.Contains(query, "?") {
		return nil, errors.New("a character that must be percent-encoded (a second ?)")
	}
	pa, err := attrs(path, ";", pathAttrs, "path")
	if err != nil {
		return nil, err
	}
	qa, err := attrs(query, "&", queryAttrs, "query")
	if err != nil {
		return nil, err
	}
	p := &PKCS11{}
	if v, ok := pa["token"]; ok {
		if p.TokenLabel, err = label("token", v); err != nil {
			return nil, err
		}
	}
	if v, ok := pa["slot-id"]; ok {
		n, perr := strconv.ParseUint(v, 10, 32)
		if perr != nil || v == "" || v[0] == '+' {
			return nil, errors.New("slot-id is not a decimal slot number")
		}
		id := uint(n)
		p.SlotID = &id
	}
	switch {
	case p.TokenLabel != "" && p.SlotID != nil:
		return nil, errors.New("give token or slot-id, not both")
	case p.TokenLabel == "" && p.SlotID == nil:
		return nil, errors.New("no token: give token=<label> (or slot-id=<number>)")
	}
	if v, ok := pa["object"]; ok {
		if p.ObjectLabel, err = label("object", v); err != nil {
			return nil, err
		}
	}
	if v, ok := pa["id"]; ok {
		if v == "" {
			return nil, errors.New("id is empty")
		}
		if len(v) > 128 {
			return nil, errors.New("id is too long")
		}
		p.ID = []byte(v)
	}
	if p.ObjectLabel == "" && len(p.ID) == 0 {
		return nil, errors.New("no key: give object=<label> or id=<bytes>")
	}
	if v, ok := pa["type"]; ok && v != "private" {
		return nil, errors.New("type must be private: bide-hsm signs with a private key")
	}
	mp, ok := qa["module-path"]
	if !ok {
		return nil, errors.New("no module-path: give ?module-path=<absolute path of the PKCS#11 library>")
	}
	if err := checkText("module-path", mp); err != nil {
		return nil, err
	}
	if mp == "" || !isAbs(mp) {
		return nil, errors.New("module-path must be an absolute path (a relative one would load whatever library the working directory holds)")
	}
	p.ModulePath = mp
	if v, ok := qa["pin-source"]; ok {
		if v == "prompt" {
			p.PINPrompt = true
		} else if v == "protected" {
			p.PINProtected = true
		} else if p.PINFile, p.PINEnv, err = pinSource(v); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// isAbs reports whether p is an absolute path: rooted at /, as a URI path is on every system, or
// absolute for the system (a drive letter on Windows).
func isAbs(p string) bool { return strings.HasPrefix(p, "/") || filepath.IsAbs(p) }

// pinSource reads pin-source: file:/absolute/path (also file:///path and file://localhost/path) or
// env:NAME.
func pinSource(v string) (file, env string, err error) {
	bad := errors.New("pin-source must be file:/absolute/path, env:NAME, prompt or protected (the PIN itself never goes in the reference)")
	switch {
	case strings.HasPrefix(v, "env:"):
		name := strings.TrimPrefix(v, "env:")
		if !approveline.ValidEnvName(name) {
			return "", "", errors.New("pin-source env: is not the name of an environment variable")
		}
		return "", name, nil
	case strings.HasPrefix(v, "file:"):
		p := strings.TrimPrefix(v, "file:")
		if rest, ok := strings.CutPrefix(p, "//"); ok {
			host, path, found := strings.Cut(rest, "/")
			if !found || (host != "" && host != "localhost") {
				return "", "", errors.New("pin-source file: names a path on this machine: file:/absolute/path")
			}
			p = "/" + path
		}
		if err := checkText("pin-source", p); err != nil {
			return "", "", err
		}
		if !isAbs(p) {
			return "", "", errors.New("pin-source file: must be an absolute path")
		}
		return p, "", nil
	}
	return "", "", bad
}

// attrs splits s into name=value attributes at sep, decodes the values and checks the names against
// allowed. A repeated name, an attribute that is not allowed and a pin-value are errors that name
// the attribute only when RFC 7512 defines it.
func attrs(s, sep string, allowed map[string]bool, where string) (map[string]string, error) {
	out := map[string]string{}
	if s == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, sep) {
		name, raw, ok := strings.Cut(part, "=")
		if !ok || name == "" {
			return nil, errors.New("malformed attribute (want name=value)")
		}
		if name == "pin-value" {
			return nil, errors.New("pin-value is refused: a PIN in the reference would sit in the process list and the shell history; use pin-source=file:/path or env:NAME")
		}
		if !allowed[name] {
			if known[name] {
				return nil, fmt.Errorf("attribute %q is not supported in the %s (bide-hsm selects a key by token or slot-id, and object or id, and loads module-path)", name, where)
			}
			return nil, errors.New("an attribute that is not supported (its name is not an RFC 7512 attribute)")
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("attribute %q is given twice", name)
		}
		v, err := decode(raw)
		if err != nil {
			return nil, fmt.Errorf("attribute %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// rawOK reports whether c may appear unencoded in a value: the unreserved characters and the
// sub-delimiters that are not this syntax's separators or its '='.
func rawOK(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~/:@!$'()*+,[]", c) >= 0
}

// decode percent-decodes a raw value, refusing a character that must have been encoded and a
// malformed escape.
func decode(raw string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c != '%' {
			if !rawOK(c) {
				return "", errors.New("holds a character that must be percent-encoded")
			}
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(raw) {
			return "", errors.New("malformed percent escape")
		}
		h, ok := unhex(raw[i+1], raw[i+2])
		if !ok {
			return "", errors.New("malformed percent escape")
		}
		b.WriteByte(h)
		i += 2
	}
	return b.String(), nil
}

func unhex(a, b byte) (byte, bool) {
	hv := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		case c >= 'A' && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	x, ok1 := hv(a)
	y, ok2 := hv(b)
	return x<<4 | y, ok1 && ok2
}

// checkText refuses text that is not UTF-8 or that holds a control, bidirectional or invisible
// character: a label is shown to a person, who must be able to read what it names.
func checkText(name, v string) error {
	if !utf8.ValidString(v) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	for _, r := range v {
		if approveline.Unsafe(r) {
			return fmt.Errorf("%s holds a control, bidirectional or invisible character", name)
		}
	}
	return nil
}

func label(name, v string) (string, error) {
	if err := checkText(name, v); err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("%s is empty", name)
	}
	return v, nil
}

// String names the key for display: the scheme, the token and the key, the module, never the
// source of a PIN.
func (r Ref) String() string {
	switch r.Kind {
	case KindAWSKMS:
		return "awskms:" + r.KMS.ARN
	case KindGCPKMS:
		return "gcpkms:" + r.GCP.Version
	case KindPKCS11:
		p := r.PKCS11
		var parts []string
		if p.TokenLabel != "" {
			parts = append(parts, "token="+encode([]byte(p.TokenLabel)))
		} else {
			parts = append(parts, "slot-id="+strconv.FormatUint(uint64(*p.SlotID), 10))
		}
		if p.ObjectLabel != "" {
			parts = append(parts, "object="+encode([]byte(p.ObjectLabel)))
		}
		if len(p.ID) > 0 {
			parts = append(parts, "id="+encodeAll(p.ID))
		}
		return "pkcs11:" + strings.Join(parts, ";") + "?module-path=" + encode([]byte(p.ModulePath))
	}
	return "(invalid key reference)"
}

// encode percent-encodes everything but the unreserved characters (and the path separator).
func encode(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~/", c) >= 0 {
			s.WriteByte(c)
		} else {
			fmt.Fprintf(&s, "%%%02x", c)
		}
	}
	return s.String()
}

func encodeAll(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		fmt.Fprintf(&s, "%%%02x", c)
	}
	return s.String()
}
