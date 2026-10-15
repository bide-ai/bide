package audit

import (
	"fmt"
	"regexp"
	"strings"
)

// IDKind names the kind of identifier that is an approver's seat. The seat is the identifier, so what
// it is matters more than its syntax: it must be immutable, never reassigned, one per natural person
// (docs/design/approver-identity.md, "How an approver id binds to a certificate").
type IDKind string

// The identifier kinds.
const (
	// IDPersonURI is an opaque person id in a URI subject alternative name that has a host
	// ("https://idp.bank.example/person/8f3a..."), a stable directory key and not a name. It is the
	// recommended kind.
	IDPersonURI IDKind = "person-uri"
	// IDEmail is an rfc822Name. Use it only when the bank states and evidences that its mailboxes are
	// one per person, never aliased and never reassigned after a leaver; otherwise the address is
	// informational and not the seat.
	IDEmail IDKind = "email"
)

func (k IDKind) valid() bool { return k == IDPersonURI || k == IDEmail }

// Canonicalizer maps an identifier to its one canonical form. It is used in every place an identity
// is compared: the identity map, the certificate's subject alternative name, the requester principal
// of a run and the identity of a bare-key approver, so two spellings of one identifier cannot be two
// seats. Canonical is a pure function: it returns the same result for the same input, and an error
// for anything outside the bank's profile. The verdict records Name and Version, so an auditor knows
// which function produced the form.
type Canonicalizer interface {
	// Name identifies the function (for the identity record).
	Name() string
	// Version changes when the function's results do.
	Version() int
	// Canonical returns the canonical form of s as an identifier of the given kind, or an error when
	// s is not in the accepted profile.
	Canonical(kind IDKind, s string) (string, error)
}

// CanonicalConfig configures the default canonicalizer (NewCanonicalizer).
type CanonicalConfig struct {
	// Scheme is the one URI scheme accepted. Empty means "https".
	Scheme string
	// Host is the one URI host accepted, in lowercase. Empty means no URI identifier is accepted.
	Host string
	// Path matches the URI path (the part after the host, starting with "/"). Empty means the
	// default: "/person/" followed by 32 lowercase hex digits. It is matched against the whole
	// path, whatever anchors the expression has. Configure exactly one spelling of the id.
	Path *regexp.Regexp
}

// defaultPersonPath is one spelling of a 128-bit person id: 32 lowercase hex digits. The dashed UUID
// rendering of the same value is a second spelling, so it is not accepted by default; a bank whose
// ids are dashed UUIDs configures that pattern instead (and only that).
var defaultPersonPath = regexp.MustCompile(`^/person/[0-9a-f]{32}$`)

// NewCanonicalizer returns the default canonicalizer, "bide.default" version 1.
//
// Email: ASCII only (an internationalized address is refused until a bank needs one), exactly one
// "@", a local part of RFC 5322 atext characters and dots (no leading, trailing or doubled dot), a
// domain of letter-digit-hyphen labels, and the whole address lowercased (the bank states that its
// directory is case-insensitive). Nothing is trimmed and no alias is folded: a.smith and asmith are
// two identifiers.
//
// URI (a strict profile, because a CA that could issue one person's identifier in two spellings
// would give one person two seats): the configured scheme, no userinfo, no port, no query, no
// fragment, no percent-encoding, no trailing slash, no dot segments, a host equal to the configured
// lowercase host, and a path that matches the configured pattern. A URI is returned unchanged:
// there is one accepted spelling, not a normalisation.
func NewCanonicalizer(cfg CanonicalConfig) Canonicalizer {
	c := &defaultCanonicalizer{scheme: cfg.Scheme, host: cfg.Host, path: cfg.Path}
	if c.scheme == "" {
		c.scheme = "https"
	}
	if c.path == nil {
		c.path = defaultPersonPath
	} else {
		// Force a whole-path match: an unanchored expression, or an alternation that matches a
		// prefix, must not accept extra text around the id.
		c.path = regexp.MustCompile(`^(?:` + c.path.String() + `)$`)
	}
	return c
}

type defaultCanonicalizer struct {
	scheme, host string
	path         *regexp.Regexp
}

func (*defaultCanonicalizer) Name() string { return "bide.default" }
func (*defaultCanonicalizer) Version() int { return 1 }

const maxIdentifierLen = 512

func (c *defaultCanonicalizer) Canonical(kind IDKind, s string) (string, error) {
	if len(s) > maxIdentifierLen {
		return "", fmt.Errorf("identifier is %d bytes, over the %d byte limit", len(s), maxIdentifierLen)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "", fmt.Errorf("identifier has a byte outside printable ASCII at offset %d (internationalized, spaced or control characters are refused)", i)
		}
	}
	switch kind {
	case IDEmail:
		return canonicalEmail(s)
	case IDPersonURI:
		return c.canonicalURI(s)
	}
	return "", fmt.Errorf("unknown identifier kind %q", kind)
}

func canonicalEmail(s string) (string, error) {
	local, domain, ok := strings.Cut(s, "@")
	if !ok || strings.Contains(domain, "@") {
		return "", fmt.Errorf("an email address has exactly one @")
	}
	if local == "" || len(local) > 64 || domain == "" || len(domain) > 253 {
		return "", fmt.Errorf("an email address has a local part of 1 to 64 bytes and a domain of 1 to 253")
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return "", fmt.Errorf("the local part has a leading, trailing or doubled dot")
	}
	for i := 0; i < len(local); i++ {
		if !isAtext(local[i]) && local[i] != '.' {
			return "", fmt.Errorf("the local part has the character %q, outside the accepted set", local[i])
		}
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("the domain has an empty, overlong or hyphen-edged label")
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", fmt.Errorf("the domain has the character %q, outside letters, digits and hyphen", ch)
			}
		}
	}
	return strings.ToLower(s), nil
}

func isAtext(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-/=?^_`{|}~", b) >= 0
}

func (c *defaultCanonicalizer) canonicalURI(s string) (string, error) {
	if c.host == "" {
		return "", fmt.Errorf("no URI host is configured, so no URI identifier is accepted")
	}
	if c.host != strings.ToLower(c.host) {
		return "", fmt.Errorf("the configured URI host is not lowercase")
	}
	rest, ok := strings.CutPrefix(s, c.scheme+"://")
	if !ok {
		return "", fmt.Errorf("the URI does not start with %q", c.scheme+"://")
	}
	if strings.ContainsAny(rest, "@?#%\\") {
		return "", fmt.Errorf("the URI has userinfo, a query, a fragment, percent-encoding or a backslash")
	}
	host, path, _ := strings.Cut(rest, "/")
	path = "/" + path
	if host != c.host {
		return "", fmt.Errorf("the URI host is not the configured host")
	}
	if strings.HasSuffix(path, "/") || strings.Contains(path, "//") || strings.Contains(path, "/./") || strings.Contains(path, "/../") {
		return "", fmt.Errorf("the URI path has a trailing slash, an empty segment or a dot segment")
	}
	if !c.path.MatchString(path) {
		return "", fmt.Errorf("the URI path does not match the configured pattern")
	}
	return s, nil
}
