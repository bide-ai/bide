package identitypreflight

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/crlrefresh"
	"github.com/bide-ai/bide/internal/policyfile"
)

// Options says what to check. Policy and Params are required, and so is a stance on CRL status
// (CRLDir, Fetch or NoStatus) and on the gate's record (Expect or NoExpect): a pre-flight never reports
// ready with a part of the setup it was not asked to look at.
type Options struct {
	// Policy is the certificate policy file; Params the declaration of the CA timings (the file bide
	// identity fetch-crl takes).
	Policy, Params string
	// Expect is the gate's recorded policy (the identity record's "policy" member, or the whole record)
	// to compare the declared values with; NoExpect acknowledges that it is not compared.
	Expect string
	// ExpectDigest is the gate's policy digest (the pause note's identity_policy_digest, or the identity
	// record's policy_digest), lowercase hex. It is compared with the digest of the declaration only when
	// the -params file states every member the digest covers; otherwise the check fails naming the missing
	// members. It can stand in for Expect, or be given with it.
	ExpectDigest string
	NoExpect     bool
	// CRLDir is a directory of CRLs read without network: a crlrefresh archive, or a flat directory of CRL
	// files. Fetch asks the policy's crl sources over HTTP(S) with the crlrefresh fetch code, storing
	// nothing; with CRLDir the directory seeds the rollback memory. NoStatus acknowledges that CRL status
	// is not checked.
	CRLDir   string
	Fetch    bool
	NoStatus bool
	// Cert is an approver's certificate chain, leaf first (PEM); Chain holds more certificates to carry
	// behind it. As is the approver id; optional when the policy names exactly one.
	Cert, Chain, As string
	// At is the time to judge at (default now). It cannot be set with Fetch.
	At time.Time
	// Client is the HTTP client for Fetch (the proxy from the environment, for the command); nil uses the
	// library's.
	Client *http.Client
}

func (o Options) validate() error {
	bad := func(format string, a ...any) error { return &UsageError{Msg: fmt.Sprintf(format, a...)} }
	switch {
	case o.Policy == "":
		return bad("-policy is required")
	case o.Params == "":
		return bad("-params is required")
	case o.NoStatus && (o.CRLDir != "" || o.Fetch):
		return bad("-no-status says CRL status is not checked; it cannot be combined with -crl-dir or -fetch")
	case !o.NoStatus && o.CRLDir == "" && !o.Fetch:
		return bad("one of -crl-dir (offline), -fetch (online) or -no-status (CRL status not checked, and the result is partial) is required")
	case (o.Expect != "" || o.ExpectDigest != "") && o.NoExpect:
		return bad("-no-expect says the gate's recorded policy is not compared; it cannot be combined with -expect or -expect-digest")
	case o.Expect == "" && o.ExpectDigest == "" && !o.NoExpect:
		return bad("-expect <file> (the gate's recorded policy), -expect-digest <hex> (its policy digest) or -no-expect (not compared, and the result is partial) is required")
	case o.ExpectDigest != "" && !validDigest(o.ExpectDigest):
		return bad("-expect-digest must be 64 lowercase hex digits (a SHA-256)")
	case o.Fetch && !o.At.IsZero():
		return bad("-at cannot be combined with -fetch: a fetched CRL is judged by the clock")
	case o.Cert == "" && (o.Chain != "" || o.As != ""):
		return bad("-chain and -as need -cert")
	}
	return nil
}

// Run performs the checks. The error is an *UsageError for a mistake in o, an *IOError for a file that
// cannot be read, and otherwise nil: a setup that is wrong is a report with failed checks, not an error.
func Run(ctx context.Context, o Options) (*Report, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	now := time.Now
	if !o.At.IsZero() {
		at := o.At
		now = func() time.Time { return at }
	}
	r := &Report{CheckedAt: now().UTC(), Partial: o.NoStatus || o.NoExpect}
	s := &run{o: o, r: r, now: now}
	if err := s.policy(); err != nil {
		return nil, err
	}
	if s.pol != nil {
		if err := s.declared(); err != nil {
			return nil, err
		}
	}
	if s.ap != nil {
		if err := s.status(ctx); err != nil {
			return nil, err
		}
	}
	if o.Cert != "" {
		if err := s.certificate(); err != nil {
			return nil, err
		}
	} else {
		r.add("cert.none", Skip, "run with -cert <chain.pem> -as <approver id> for each approver before go-live", "no approver certificate was checked: this is a check of the deployment's setup, not of any approver's certificate")
	}
	r.finish()
	return r, nil
}

type run struct {
	o   Options
	r   *Report
	now func() time.Time

	pol     *audit.CertIdentity
	src     audit.CRLSources
	inter   []*x509.Certificate
	decl    audit.StatusDecl
	snap    *audit.StatusSnapshot
	ap      *audit.CertApprover
	names   map[audit.SPKIHash]string
	refresh *crlrefresh.Refresher
	haveCRL bool // status was loaded or fetched (a refresher ran)
}

func ioErr(flag string, err error) error { return &IOError{Err: fmt.Errorf("%s: %w", flag, err)} }

// policy loads the policy file (a) and checks the pinned CAs and intermediates (b).
func (s *run) policy() error {
	if _, err := os.ReadFile(s.o.Policy); err != nil {
		return ioErr("-policy", err)
	}
	r := s.r
	pol, err := policyfile.Read(s.o.Policy)
	if err != nil {
		r.add("policy.load", Fail, "correct the policy file; its error names the member (the file's members are listed in the certificate approvers guide)", "%v", err)
		return nil
	}
	r.add("policy.load", OK, "", "the policy loads: %d approver id(s), %d trust anchor(s), %d pinned intermediate key(s)", len(pol.ApproverIDs()), len(pol.TrustAnchors()), len(pol.PinnedIntermediates()))
	s.pol = pol
	s.names = map[audit.SPKIHash]string{}
	label := func(c *x509.Certificate) string {
		h := audit.SPKIHashOf(c)
		// Only CA certificates are labelled with their subject; an approver's leaf never is (its common name
		// is usually a person's name).
		cn := strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, c.Subject.CommonName)
		return fmt.Sprintf("%x (%s)", h[:4], cn)
	}
	now := s.now()
	for _, c := range pol.TrustAnchors() {
		s.names[audit.SPKIHashOf(c)] = label(c)
		s.certDates("policy.anchor", "anchor "+label(c), c, now)
	}
	src, inter, err := policyfile.ReadSources(s.o.Policy)
	if err != nil {
		r.add("policy.sources", Fail, "correct the crl and intermediate_certs members of the policy file", "%v", err)
		return nil
	}
	s.src, s.inter = src, inter
	have := map[audit.SPKIHash]bool{}
	for _, c := range inter {
		k := audit.SPKIHashOf(c)
		have[k] = true
		s.names[k] = label(c)
		if !pol.IsPinnedIntermediate(k) {
			r.add("policy.intermediates", Fail, "pin its key under \"intermediates\" in the policy file, or remove the file from intermediate_certs", "intermediate_certs holds %s, whose key the policy does not pin", label(c))
			continue
		}
		s.certDates("policy.intermediate", "intermediate "+label(c), c, now)
	}
	for _, k := range pol.PinnedIntermediates() {
		if !have[k] {
			r.add("policy.intermediates", Fail, "add the PEM certificate of that intermediate to \"intermediate_certs\": the policy pins a key, and a key cannot verify a CRL", "the policy pins the intermediate key %x, and intermediate_certs holds no certificate for it", k[:4])
		}
	}
	if !slices.ContainsFunc(r.Checks, func(c Check) bool { return c.ID == "policy.intermediates" }) {
		r.add("policy.intermediates", OK, "", "%d pinned intermediate key(s), each with its certificate in intermediate_certs", len(pol.PinnedIntermediates()))
	}
	return nil
}

// certDates reports a CA certificate's validity: expired is a failure (every chain through it is
// refused), under 90 days is a warning.
func (s *run) certDates(id, what string, c *x509.Certificate, now time.Time) {
	switch {
	case now.After(c.NotAfter):
		s.r.add(id, Fail, "replace it (a new CA certificate pinned in the policy) before any decision can count", "%s expired %s: every chain through it is refused", what, c.NotAfter.UTC().Format(time.RFC3339))
	case now.Before(c.NotBefore):
		s.r.add(id, Fail, "check the clock of this host, or the certificate", "%s is not valid until %s", what, c.NotBefore.UTC().Format(time.RFC3339))
	case c.NotAfter.Sub(now) < 90*24*time.Hour:
		s.r.add(id, Warn, "plan its replacement now: a CA change needs a policy change, a new certificate pinned, and a change record", "%s expires %s (%d days)", what, c.NotAfter.UTC().Format("2006-01-02"), int(c.NotAfter.Sub(now).Hours()/24))
	default:
		s.r.add(id, OK, "", "%s valid until %s", what, c.NotAfter.UTC().Format("2006-01-02"))
	}
}

// declared loads -params (c, the config half), validates it with the gate's own ValidateConfig, and
// compares it with the gate's record (d).
func (s *run) declared() error {
	r := s.r
	pb, err := os.ReadFile(s.o.Params)
	if err != nil {
		return ioErr("-params", err)
	}
	decl, err := audit.ParseStatusDecl(s.o.Params, pb)
	if err != nil {
		r.add("params.load", Fail, "correct the -params file; it is a copy of the timings the gate declares (CAPeriod, MaxStatusAge, ExecStatusAge, ThisUpdateSkew, and per issuer ...For values)", "%v", err)
		return nil
	}
	r.add("params.load", OK, "", "the declared CA timings load")
	s.decl = decl
	s.snap = audit.NewStatusSnapshot(audit.WithIssuers(append(s.pol.TrustAnchors(), s.inter...)...))
	ap, err := decl.Approver(s.pol, s.snap, s.now)
	if err != nil {
		r.add("params.config", Fail, "the policy needs at least one approver id in its identity map", "%v", err)
		return nil
	}
	if err := ap.ValidateConfig(); err != nil {
		r.add("params.config", Fail, "make the -params values agree with the rules the gate enforces at start-up (ExecStatusAge at least twice the CA period; a declared tier needs ca_period, ca_overlap and max_status_age); the gate refuses to start otherwise", "%v", err)
		return nil
	}
	r.add("params.config", OK, "", "the declared timings pass the gate's own configuration check (CertApprover.ValidateConfig)")
	s.ap = ap
	if s.o.NoExpect {
		r.add("params.expect", Skip, "run again with -expect <the gate's recorded policy> before go-live", "the declared values were not compared with the gate's recorded policy (-no-expect)")
		return nil
	}
	if s.o.ExpectDigest != "" {
		s.digest(decl)
	}
	if s.o.Expect == "" {
		return nil
	}
	rec, err := readRecord(s.o.Expect)
	if err != nil {
		var ie *IOError
		if errors.As(err, &ie) {
			return err
		}
		r.add("params.expect", Fail, "give -expect the identity record's \"policy\" member, or the whole ApprovalIdentity record, as journaled by a gate of this bide version", "%v", err)
		return nil
	}
	diffs, unverified := compareRecords(standIn(ap), rec, decl.MissingForDigest())
	for _, d := range diffs {
		r.add("params.expect", Fail, "make the policy file and -params carry the values the gate runs with, or the gate's declaration the ones you checked; the schedule and the alarm follow these numbers", "%s", d)
	}
	if len(diffs) == 0 {
		r.add("params.expect", OK, "", "the policy file and -params agree with the gate's recorded policy on every member they can declare")
	}
	if len(unverified) > 0 {
		r.add("params.expect.unverified", OK, "", "not verified, because the -params file does not state them (state them in the file, or compare them with the gate's declaration by hand): %s", join(unverified))
	}
	return nil
}

func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// digest compares the digest of the declaration with the gate's. It is an equality only when the file states
// every member the digest covers; otherwise the missing members would take defaults and the answer would
// mean nothing, so the check fails and names them.
func (s *run) digest(decl audit.StatusDecl) {
	if missing := decl.MissingForDigest(); len(missing) > 0 {
		s.r.add("params.digest", Fail, "state these members in the -params file (each with the value the gate declares; see the certificate approvers guide), or use -expect to compare the members that are stated", "the -params file lacks %s, so its policy digest cannot be compared with the gate's", join(missing))
		return
	}
	got := s.ap.PolicyDigest()
	if got == s.o.ExpectDigest {
		s.r.add("params.digest", OK, "", "the policy file and -params produce the gate's policy digest %s", got)
		return
	}
	s.r.add("params.digest", Fail, "the file and the gate declare different values; run again with -expect (the gate's recorded policy) to see which members differ", "the policy file and -params produce policy digest %s, the gate's is %s", got, s.o.ExpectDigest)
}

// status builds the refresher over the declared values (c): its construction is the gate-side matching of
// sources, issuer certificates and schedule margins, and loading or fetching is the same code a
// fetch-crl job runs.
func (s *run) status(ctx context.Context) error {
	r := s.r
	if s.src == nil {
		r.add("crl.setup", Skip, "fix policy.sources above first", "not run: the policy's crl and intermediate_certs members did not load")
		return nil
	}
	var mu sync.Mutex
	var events []crlrefresh.Event
	opts := crlrefresh.Options{Issuers: s.inter, DeltaPeriodFor: s.decl.DeltaPeriodFor, Client: s.o.Client, StandIn: &crlrefresh.StandIn{Missing: s.decl.MissingForDigest()},
		Hooks: crlrefresh.Hooks{OnEvent: func(e crlrefresh.Event) { mu.Lock(); events = append(events, e); mu.Unlock() }}}
	flat := false
	if s.o.CRLDir != "" {
		arch, err := isArchive(s.o.CRLDir)
		if err != nil {
			return err
		}
		if arch {
			opts.Archive = readOnlyArchive{crlrefresh.DirArchive(s.o.CRLDir)}
		} else {
			flat = true
		}
	}
	ref, err := crlrefresh.New(s.ap, s.src, opts)
	if err != nil {
		r.add("crl.setup", Fail, "give every pinned anchor and intermediate a \"crl\" source (hex SPKI hash of the issuer), and declare timings that leave the refresher a margin before gates pause", "%v", err)
		if s.o.NoStatus {
			r.add("crl.status", Skip, "", "CRL status was not checked (-no-status)")
		}
		return nil
	}
	s.refresh = ref
	r.add("crl.setup", OK, "", "every pinned issuer has a CRL source and a certificate, and the declared timings leave a schedule margin")
	if s.o.NoStatus {
		r.add("crl.status", Skip, "run again with -crl-dir or -fetch before go-live", "CRL status was not checked (-no-status)")
		return nil
	}
	switch {
	case flat:
		n, rejected, err := loadFlat(s.o.CRLDir, s.snap)
		if err != nil {
			return err
		}
		for _, m := range rejected {
			r.add("crl.file", Warn, "remove the file, or fetch the CRL again from the CA", "not used: %s", m)
		}
		r.add("crl.load", OK, "", "%d CRL(s) read from %s and verified against the pinned issuers", n, s.o.CRLDir)
	case s.o.Fetch:
		for _, res := range ref.RefreshOnce(ctx) {
			s.fetchResult(res, events, &mu)
		}
	default:
		if err := ref.Seed(ctx); err != nil {
			r.add("crl.load", Fail, "restore the archive from backup, or fetch the CRLs again", "reading the archive %s: %v", s.o.CRLDir, err)
		} else {
			r.add("crl.load", OK, "", "the archive %s was read without writing to it", s.o.CRLDir)
		}
	}
	s.haveCRL = true
	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if isSecurity(e.Kind) {
			r.add("crl.event."+string(e.Kind), Warn, "see the CRL refresher section of the certificate approvers guide for this event", "%s: %s", s.issuerName(e.Issuer), eventText(e))
		}
	}
	for _, h := range ref.Health() {
		s.fresh(h)
	}
	return nil
}

func eventText(e crlrefresh.Event) string {
	t := e.Detail
	if e.Err != nil {
		t = cmpText(t, e.Err.Error())
	}
	return t
}

func cmpText(a, b string) string {
	if a == "" {
		return b
	}
	return a + ": " + b
}

func (s *run) issuerName(k audit.SPKIHash) string {
	if n, ok := s.names[k]; ok {
		return n
	}
	return fmt.Sprintf("%x", k[:4])
}

// fetchResult reports one issuer's online pass: the per-URL outcomes when it failed.
func (s *run) fetchResult(res crlrefresh.IssuerResult, events []crlrefresh.Event, mu *sync.Mutex) {
	name := s.issuerName(res.Issuer)
	if res.Err == nil {
		s.r.add("crl.fetch", OK, "", "%s: a CRL was fetched or confirmed", name)
		return
	}
	mu.Lock()
	detail := ""
	for _, e := range events {
		if e.Issuer == res.Issuer && e.Kind == crlrefresh.EventFetchFailed {
			detail += fmt.Sprintf("; %s: %s", e.URL, e.Detail)
		}
	}
	mu.Unlock()
	s.r.add("crl.fetch", Fail, "make the CRL URL reachable from this host (firewall, proxy, DNS, TLS trust) and serving the CRL the CA signs; only http and https are fetched, and a redirect must stay on the same host", "%s: %v%s", name, res.Err, detail)
}

// fresh reports one issuer's freshness from the refresher's own health model, which uses the status
// rule's freshness test.
func (s *run) fresh(h crlrefresh.IssuerHealth) {
	id := "crl.fresh." + fmt.Sprintf("%x", h.Issuer[:4])
	name := s.issuerName(h.Issuer)
	switch {
	case h.NewestThisUpdate.IsZero():
		s.r.add(id, Fail, "fetch the CRL (bide identity fetch-crl) or put it in -crl-dir; a gate with no CRL for a pinned issuer pauses every decision", "%s: no CRL is held", name)
	case h.Critical:
		s.r.add(id, Fail, "the CA must publish a newer CRL, or the fetch job is not running; a gate would pause on this status now", "%s: the newest CRL (number %s, thisUpdate %s, nextUpdate %s) is %s old, past what the declared ages accept (limit %s): a gate would not accept it",
			name, h.NewestNumber, h.NewestThisUpdate.UTC().Format(time.RFC3339), h.NewestNextUpdate.UTC().Format(time.RFC3339), h.Age.Round(time.Second), h.Limit)
	case h.Alarm:
		s.r.add(id, Warn, "the CRL is older than the refresher's alarm age: check that the CA publishes within its declared period and that the fetch job runs", "%s: the newest CRL (number %s, thisUpdate %s) is %s old, past the alarm age %s (limit %s)",
			name, h.NewestNumber, h.NewestThisUpdate.UTC().Format(time.RFC3339), h.Age.Round(time.Second), h.AlarmAge, h.Limit)
	default:
		s.r.add(id, OK, "", "%s: CRL number %s, thisUpdate %s, nextUpdate %s, %s old, %s inside the limit of %s", name, h.NewestNumber,
			h.NewestThisUpdate.UTC().Format(time.RFC3339), h.NewestNextUpdate.UTC().Format(time.RFC3339), h.Age.Round(time.Second), (h.Limit - h.Age).Round(time.Second), h.Limit)
	}
	if h.UnackedResets > 0 {
		s.r.add(id+".resets", Warn, "acknowledge them with bide identity ack-reset after review", "%s: %d high-water reset record(s) in the archive are not acknowledged", name, h.UnackedResets)
	}
}

func isStatusReason(reason string) bool {
	return reason == agent.ReasonRevoked || reason == agent.ReasonStatusUnknown || reason == agent.ReasonStatusStale
}

// certificate answers (e): would the gate accept this chain for the approver, now. The chain is judged as
// a decision signed and provable at the check time, by the function the gate's counting and execution
// checks call. The signature is not checked (no decision exists).
func (s *run) certificate() error {
	r := s.r
	der, problem, err := readChain(s.o.Cert, "-cert")
	if err != nil {
		return err
	}
	if problem == "" && s.o.Chain != "" {
		var more [][]byte
		if more, problem, err = readChain(s.o.Chain, "-chain"); err != nil {
			return err
		}
		der = append(der, more...)
	}
	if problem != "" {
		r.add("cert.parse", Fail, "give -cert the approver's certificate chain as PEM, leaf first, as bide approve -cert takes it", "%s", problem)
		return nil
	}
	r.add("cert.parse", OK, "", "%d certificate(s) read; the leaf is first", len(der))
	if s.ap == nil || s.pol == nil {
		r.add("cert.chain", Skip, "fix the policy and -params checks above first", "the certificate was not judged: the policy or the declared values did not load")
		return nil
	}
	as := s.o.As
	ids := s.pol.ApproverIDs()
	switch {
	case as == "" && len(ids) == 1:
		as = ids[0]
	case as == "":
		return &UsageError{Msg: fmt.Sprintf("-as is required: the policy names %d approver ids (%s)", len(ids), join(ids))}
	case !slices.Contains(ids, as):
		return &UsageError{Msg: fmt.Sprintf("-as %q is not an approver id of the policy (%s)", as, join(ids))}
	}
	ap, err := s.decl.Approver(s.pol, s.snap, s.now)
	if err != nil {
		return nil
	}
	ap.ApproverID, ap.ExecNow = as, s.now
	at := s.now()
	count := ap.CheckCertificate(der, at, at, audit.CheckCount)
	if count.Reason != "" && !isStatusReason(count.Reason) {
		r.add("cert.chain", Fail, chainFix(count.Reason), "%s: %s", count.Reason, count.Detail)
		return nil
	}
	r.add("cert.chain", OK, "", "accepted for approver %q as %s; the verified path has %d certificate(s), the anchor included, and ends at the anchor with key %s", as, count.Identity, len(count.Chain), short32(count.Anchor))
	if s.o.NoStatus || !s.haveCRL {
		r.add("cert.status.count", Skip, "run again with -crl-dir or -fetch", "revocation status was not checked (-no-status), so whether the decision would count was not judged")
		r.add("cert.status.exec", Skip, "run again with -crl-dir or -fetch", "revocation status was not checked (-no-status), so whether the call would run was not judged")
		return nil
	}
	s.statusLine("cert.status.count", "a decision made now would count", count)
	s.statusLine("cert.status.exec", "the call would pass the check before it runs", ap.CheckCertificate(der, at, at, audit.CheckExec))
	return nil
}

func short32(hexStr string) string {
	if len(hexStr) > 8 {
		return hexStr[:8]
	}
	return hexStr
}

func (s *run) statusLine(id, ok string, c agent.CertCheck) {
	if c.Reason == "" {
		s.r.add(id, OK, "", "%s (%s, status time %s, basis: %s)", ok, c.Tier, c.StatusTime, c.Basis)
		return
	}
	s.r.add(id, Fail, statusFix(c.Reason), "%s: %s", c.Reason, c.Detail)
}
