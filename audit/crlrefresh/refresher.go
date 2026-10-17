// Package crlrefresh keeps the status snapshot of a certificate-bound approver (audit.CertApprover)
// supplied with the newest verified CRLs of the issuing CAs its policy pins, and archives every CRL it
// accepts (docs/design/crl-refresher.md, docs/guides/certificate-approvers.md).
//
// It does the network and file I/O that package audit must not: it fetches only from URLs the operator
// configured (audit.CRLSources), verifies each CRL against its own issuer's certificate with
// audit.VerifyCRL before adding it, refuses a rollback or an equivocation, and never makes status look
// fresher than the CRLs say, because freshness is read only from each CRL's own dates (audit.StatusFresh,
// the status rule's own test). The verdicts stay in audit.CertIdentity.EvaluateStatus.
package crlrefresh

import (
	"context"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/bide-ai/bide/audit"
	"math/big"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Refresher keeps the audit.StatusSnapshot of a audit.CertApprover supplied with the newest verified CRLs of the
// issuing CAs the certificate policy pins, and archives every CRL it accepts (docs/design/crl-refresher.md).
//
// It fetches only from URLs the operator configured, never from a client-supplied chain and never in the
// request path. It verifies a CRL against the issuer's own certificate before adding it, never replaces
// a newer CRL with an older one, and cannot make status look fresher than the CRLs say: freshness is
// read from each CRL's thisUpdate and nextUpdate, never from a transport header. A failed refresh
// changes nothing, so a dead refresher or CA ends in the gate's existing outcome (a pause).
//
// It is not part of the verifier: audit.CertApprover stays free of I/O.
type Refresher struct {
	appr    *audit.CertApprover
	client  *http.Client
	archive Archive
	hooks   Hooks
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	rnd     func() float64

	issuers []*issuerRT
	byKey   map[audit.SPKIHash]*issuerRT

	mu         sync.Mutex // validators, ready, hooks serialisation is not needed
	validators map[string]validator
	readyCh    chan struct{}
	readyDone  bool
	running    bool
	maxQueue   int
	futureAt   map[audit.SPKIHash]time.Time
	skewWarned time.Time
}

// Options configures New.
type Options struct {
	// Issuers are the certificates of the pinned intermediates (the policy pins them by key only). Each
	// must match a pinned key and chain to an anchor; a pinned intermediate with no certificate here is
	// refused. They are the only certificates a CRL signature is checked against besides the anchors.
	Issuers []*x509.Certificate
	// DeltaPeriodFor is the declared delta CRL period of each issuer that has DeltaURLs.
	DeltaPeriodFor map[audit.SPKIHash]time.Duration
	// Client is the application's HTTP client (mTLS, a proxy, a resolver). Nil builds the library's: no
	// proxy, no compression, timeouts on every stage. The library cannot enforce proxy or DNS rules on a
	// client it did not build; it still bounds the body, the context and the redirects.
	Client *http.Client
	// Archive stores accepted CRLs and the rollback memory. Nil runs without: a restart forgets rollback
	// protection, and the refresher says so at start.
	Archive Archive
	// Hooks receives events and alarms.
	Hooks Hooks
	// Sleep waits for d or the context; nil uses a timer. Rand returns [0,1); nil uses math/rand/v2.
	// Both exist so a test can drive the schedule without a real clock.
	Sleep func(ctx context.Context, d time.Duration) error
	Rand  func() float64
	// MaxArchiveQueueBytes bounds the memory held for CRLs the archive refused (default 64 MiB); the
	// oldest are dropped and reported.
	MaxArchiveQueueBytes int
}

// Hooks are called from the refresher's goroutines, possibly concurrently for different
// issuers. They must not block; a panic in one is recovered.
type Hooks struct {
	OnEvent func(Event)
	OnAlarm func(Alarm)
}

// EventKind names what an Event reports.
type EventKind string

// The events. Rollback, conflict, bad-signature, distrusted, future-dated, unsupported, archive-dropped
// and high-water-reset are security-relevant and should reach the operator.
const (
	EventStartup          EventKind = "startup"
	EventFetched          EventKind = "fetched"
	EventUnchanged        EventKind = "unchanged"
	EventFetchFailed      EventKind = "fetch-failed"
	EventRollback         EventKind = "rollback"
	EventConflict         EventKind = "conflict"
	EventBadSignature     EventKind = "bad-signature"
	EventDistrusted       EventKind = "distrusted"
	EventUnsupportedCRL   EventKind = "unsupported-crl"
	EventFutureDated      EventKind = "future-dated"
	EventStaleOnArrival   EventKind = "stale-on-arrival"
	EventArchiveError     EventKind = "archive-error"
	EventArchiveDropped   EventKind = "archive-dropped"
	EventArchiveCorrupt   EventKind = "archive-corrupt"
	EventClockSkew        EventKind = "clock-skew"
	EventHighWaterReset   EventKind = "high-water-reset"
	EventSetAsideServed   EventKind = "set-aside-served"
	EventSourcesChanged   EventKind = "sources"
	EventPanic            EventKind = "panic"
	EventSeedStopped      EventKind = "seed-stopped"
	EventUnpairedDelta    EventKind = "unpaired-delta"
	EventRedirectRefused  EventKind = "redirect-refused"
	EventOversizeResponse EventKind = "oversize"
)

// Event is one thing the refresher did or refused. URLs carry no query or fragment; no event holds a
// response body.
type Event struct {
	Kind     EventKind
	Issuer   audit.SPKIHash
	Feed     string // "base" or "delta"
	URL      string
	Detail   string
	Number   *big.Int // the CRL's number
	Held     *big.Int // the issuer's high-water mark, for a rollback or conflict
	Hash     [32]byte
	Err      error
	At       time.Time
	Outcomes []URLOutcome // per URL, for a fetched or failed attempt
}

// Alarm is a change in an issuer's alarm state.
type Alarm struct {
	Issuer    audit.SPKIHash
	Active    bool          // the alarm is on
	Critical  bool          // a gate would not accept the newest CRL now (gates are pausing)
	Age       time.Duration // now - thisUpdate of the newest CRL; zero when none is held
	Threshold time.Duration // the age beyond which the alarm is on
	Limit     time.Duration
	At        time.Time
	Detail    string
}

// IssuerHealth is the state of one issuer, from Health.
type IssuerHealth struct {
	Issuer              audit.SPKIHash
	URLs, DeltaURLs     []string // the effective list, without query or fragment
	LastAttempt         time.Time
	LastSuccess         time.Time // the last attempt that confirmed the newest CRL and found it fresh
	NewestNumber        *big.Int
	NewestThisUpdate    time.Time
	NewestNextUpdate    time.Time // the earlier of base and paired delta
	Age                 time.Duration
	NextExpected        time.Time // thisUpdate + declared skew + period
	NextAttempt         time.Time
	ConsecutiveFailures int
	LastError           string
	ArchiveBacklog      int
	Paired              bool // a delta is held and paired with its base (false when no deltas are configured)
	Alarm, Critical     bool
	Limit               time.Duration
	AlarmAge            time.Duration
	Loop                time.Time // the last time this issuer's loop ran: a watchdog for a dead loop
	UnackedResets       int
}

// issuerRT is the runtime state of one issuing CA.
type issuerRT struct {
	r      *Refresher
	key    audit.SPKIHash
	cert   *x509.Certificate
	timing issuerTiming
	base   *feed
	delta  *feed // nil when the CA publishes no deltas

	// resetMu guards setAside and resetSeen: a fetched CRL is checked against the set under the read lock, a
	// reset applies under the write lock.
	resetMu   sync.RWMutex
	setAside  map[string]bool // hex SHA-256 of CRLs an operator set aside
	resetSeen string          // the reset records last applied
	resetInit bool

	mu       sync.Mutex
	alarm    bool
	critical bool
	loop     time.Time
	unacked  int
	// need is how many CRLs the live rule needs of this issuer (the status age over the period, bases and
	// deltas, plus two each): the seed loads no more, so one issuer's old CRLs cannot starve another's.
	need int
}

// New builds the refresher for approver's snapshot. Every issuer reachable through an
// accepted chain (the anchors and the pinned intermediates) needs a source and a certificate; the
// approver's declared values (the CA period, the status ages, the overlap, the skew) drive the schedule
// and the alarm, so there is no second copy to drift. It returns an error wrapping agent.ErrConfig when
// the approver has no status snapshot or clock, a source is missing or malformed, an issuer certificate
// does not match a pinned key, or the declared values leave no margin for an alarm.
func New(a *audit.CertApprover, sources audit.CRLSources, opt Options) (*Refresher, error) {
	bad := func(format string, args ...any) (*Refresher, error) {
		return nil, fmt.Errorf("audit: CRL refresher: %s: %w", fmt.Sprintf(format, args...), agent.ErrConfig)
	}
	switch {
	case a == nil || a.Policy == nil:
		return bad("no audit.CertApprover with a policy")
	case a.Status == nil:
		return bad("the approver has no audit.StatusSnapshot to feed")
	case a.Now == nil:
		return bad("the approver has no Now clock: the refresher judges freshness by the same clock as the gate")
	}
	if err := a.ValidateConfig(); err != nil {
		return nil, err
	}
	if err := sources.Validate(); err != nil {
		return nil, err
	}
	pol := a.Policy
	// The issuer certificates: the anchors and the supplied intermediates.
	certs := map[audit.SPKIHash]*x509.Certificate{}
	for _, c := range pol.TrustAnchors() {
		certs[audit.SPKIHashOf(c)] = c
	}
	pool := x509.NewCertPool()
	for _, c := range opt.Issuers {
		pool.AddCert(c)
	}
	for _, c := range opt.Issuers {
		k := audit.SPKIHashOf(c)
		if !pol.IsPinnedIntermediate(k) {
			return bad("issuer certificate %x is not a pinned intermediate", k[:4])
		}
		if _, err := c.Verify(x509.VerifyOptions{Roots: pol.TrustRoots(), Intermediates: pool, CurrentTime: a.Now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
			return bad("issuer certificate %x does not chain to an anchor: %v", k[:4], err)
		}
		certs[k] = c
	}
	for _, k := range pol.PinnedIntermediates() {
		if certs[k] == nil {
			return bad("the pinned intermediate %x has no certificate in Issuers: the policy pins keys, and a key cannot verify a CRL", k[:4])
		}
	}
	for k := range sources {
		if certs[k] == nil {
			return bad("a source is named for %x, which is not a pinned issuer", k[:4])
		}
	}
	client := opt.Client
	given := client != nil
	if client == nil {
		client = newCRLClient()
	}
	r := &Refresher{appr: a, client: client, archive: opt.Archive, hooks: opt.Hooks, now: a.Now,
		sleep: opt.Sleep, rnd: opt.Rand, byKey: map[audit.SPKIHash]*issuerRT{}, validators: map[string]validator{},
		readyCh: make(chan struct{}), maxQueue: opt.MaxArchiveQueueBytes}
	if r.sleep == nil {
		r.sleep = realSleep
	}
	if r.rnd == nil {
		r.rnd = rand.Float64
	}
	if r.maxQueue <= 0 {
		r.maxQueue = 64 << 20
	}
	keys := slices.SortedFunc(mapKeys(certs), cmpHash)
	var warnings []string
	need := 0
	for _, k := range keys {
		src, ok := sources[k]
		if !ok {
			return bad("no CRL source for the issuer %x: without one every gate that depends on it would pause for good", k[:4])
		}
		urls, delta := slices.Clone(src.URLs), slices.Clone(src.DeltaURLs)
		if src.FromPinnedCDP {
			u, d, skipped := cdpURLs(certs, k)
			urls, delta = append(urls, u...), append(delta, d...)
			for _, s := range skipped {
				warnings = append(warnings, fmt.Sprintf("issuer %x: distribution point %s skipped", k[:4], s))
			}
		}
		urls, delta = dedupe(urls), dedupe(delta)
		if len(urls) == 0 {
			return bad("the issuer %x has no URL (from_pinned_cdp found none)", k[:4])
		}
		t, err := timingFor(a, k)
		if err != nil {
			return nil, err
		}
		iss := &issuerRT{r: r, key: k, cert: certs[k], timing: t}
		iss.base = &feed{iss: iss, kind: "base", urls: urls, period: t.Period}
		iv := t.interval(t.Period)
		w, err := t.checkMargin(iv)
		if err != nil {
			return nil, fmt.Errorf("audit: CRL refresher: issuer %x: %v: %w", k[:4], err, agent.ErrConfig)
		}
		if w != "" {
			warnings = append(warnings, fmt.Sprintf("issuer %x: %s", k[:4], w))
		}
		iss.need = int(t.Limit/t.Period) + 2
		need += iss.need
		if len(delta) > 0 {
			dp := opt.DeltaPeriodFor[k]
			if dp <= 0 {
				return bad("the issuer %x has delta URLs and no DeltaPeriodFor: the delta schedule needs the delta period", k[:4])
			}
			iss.delta = &feed{iss: iss, kind: "delta", urls: delta, period: dp}
			iss.need += int(t.Limit/dp) + 2
			need += int(t.Limit/dp) + 2
		}
		r.issuers = append(r.issuers, iss)
		r.byKey[k] = iss
	}
	if m := statusMax(a.Status); m > 0 && m < need {
		return bad("the status snapshot keeps %d CRLs (WithMaxCRLs) but the policy needs about %d across its issuers (the status age over the CRL period, bases and deltas, plus two each): evicting a CRL the live rule needs makes status staler", m, need)
	}
	for _, f := range r.feeds() {
		f.attemptTimeout = attemptDeadline(f.iss.timing.interval(f.period))
	}
	r.startupEvents(given, warnings)
	return r, nil
}

func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func dedupe(s []string) []string {
	var out []string
	for _, x := range s {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func (r *Refresher) feeds() []*feed {
	var out []*feed
	for _, i := range r.issuers {
		out = append(out, i.base)
		if i.delta != nil {
			out = append(out, i.delta)
		}
	}
	return out
}

// cdpURLs reads the CRL distribution points and freshest CRL URLs of the supplied certificates whose
// issuer is k. A root has none of its own; the intermediate certificate the operator supplied names the
// root's CRL.
func cdpURLs(certs map[audit.SPKIHash]*x509.Certificate, k audit.SPKIHash) (base, delta, skipped []string) {
	issuer := certs[k]
	for ck, c := range certs {
		if ck == k || c.CheckSignatureFrom(issuer) != nil {
			continue
		}
		for _, raw := range c.CRLDistributionPoints {
			if _, err := audit.CheckCRLURL(raw); err != nil {
				skipped = append(skipped, audit.RedactURL(raw))
				continue
			}
			base = append(base, raw)
		}
		for _, e := range c.Extensions {
			if e.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 46}) {
				for _, raw := range distPointURIs(e.Value) {
					if _, err := audit.CheckCRLURL(raw); err != nil {
						skipped = append(skipped, audit.RedactURL(raw))
						continue
					}
					delta = append(delta, raw)
				}
			}
		}
	}
	return base, delta, skipped
}

// distPointURIs reads the URI names of a CRLDistributionPoints-shaped extension value.
func distPointURIs(der []byte) []string {
	var dps []struct {
		DP     asn1.RawValue `asn1:"optional,tag:0"`
		Reason asn1.RawValue `asn1:"optional,tag:1"`
		Issuer asn1.RawValue `asn1:"optional,tag:2"`
	}
	if rest, err := asn1.Unmarshal(der, &dps); err != nil || len(rest) != 0 {
		return nil
	}
	var out []string
	for _, dp := range dps {
		inner := dp.DP.Bytes // [0] DistributionPointName: fullName [0] GeneralNames
		var full asn1.RawValue
		if rest, err := asn1.Unmarshal(inner, &full); err != nil || len(rest) != 0 || full.Tag != 0 {
			continue
		}
		names := full.Bytes
		for len(names) > 0 {
			var gn asn1.RawValue
			rest, err := asn1.Unmarshal(names, &gn)
			if err != nil {
				break
			}
			names = rest
			if gn.Class == asn1.ClassContextSpecific && gn.Tag == 6 {
				out = append(out, string(gn.Bytes))
			}
		}
	}
	return out
}

// startupEvents reports the effective sources and the margin of each issuer, as events so a platform
// team can alert on a margin under one period.
func (r *Refresher) startupEvents(clientGiven bool, warnings []string) {
	for _, i := range r.issuers {
		iv := i.timing.interval(i.timing.Period)
		r.emit(Event{Kind: EventSourcesChanged, Issuer: i.key, Feed: "base", URL: joinURLs(i.base.urls),
			Detail: fmt.Sprintf("urls=%v delta_urls=%v interval=%v limit=%v alarm_age=%v margin=%v",
				redactAll(i.base.urls), redactAll(i.deltaURLs()), iv, i.timing.Limit, i.timing.alarmAge(iv), i.timing.margin())})
	}
	r.emit(Event{Kind: EventStartup, Detail: "policy digest " + r.appr.PolicyDigest() + ": compare it with the digest the gate records, so a copy of the declared values (bide identity -params) that differs from the gate's is noticed"})
	if r.archive == nil {
		r.emit(Event{Kind: EventStartup, Detail: "no archive: a restart forgets rollback protection and no dated series is kept"})
	}
	if clientGiven {
		r.emit(Event{Kind: EventStartup, Detail: "an application HTTP client was supplied: proxy and DNS rules are the application's"})
	}
	for _, w := range warnings {
		r.emit(Event{Kind: EventStartup, Detail: w})
	}
}

func (i *issuerRT) deltaURLs() []string {
	if i.delta == nil {
		return nil
	}
	return i.delta.urls
}

func redactAll(s []string) []string {
	out := make([]string, len(s))
	for n, x := range s {
		out[n] = audit.RedactURL(x)
	}
	return out
}

func joinURLs(s []string) string {
	out := ""
	for n, x := range redactAll(s) {
		if n > 0 {
			out += " "
		}
		out += x
	}
	return out
}

// emit calls the event hook, recovering a panic in it.
func (r *Refresher) emit(e Event) {
	if r.hooks.OnEvent == nil {
		return
	}
	if e.At.IsZero() {
		e.At = r.now()
	}
	defer func() { _ = recover() }()
	r.hooks.OnEvent(e)
}

func (r *Refresher) alarm(a Alarm) {
	if r.hooks.OnAlarm == nil {
		return
	}
	defer func() { _ = recover() }()
	r.hooks.OnAlarm(a)
}

func (r *Refresher) setValidator(url string, v validator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v == (validator{}) {
		delete(r.validators, url)
		return
	}
	r.validators[url] = v
}

func (r *Refresher) clearValidators(urls []string) {
	for _, u := range urls {
		r.setValidator(u, validator{})
	}
}

// Ready is closed once every issuer holds a fresh CRL. Nothing requires waiting for it: until then the
// gates pause, as they do without a refresher.
func (r *Refresher) Ready() <-chan struct{} { return r.readyCh }

func (r *Refresher) checkReady() {
	r.mu.Lock()
	done := r.readyDone
	r.mu.Unlock()
	if done {
		return
	}
	now := r.now()
	for _, i := range r.issuers {
		if h := i.health(now); h.NewestThisUpdate.IsZero() || h.Critical {
			return
		}
	}
	r.mu.Lock()
	if !r.readyDone {
		r.readyDone = true
		close(r.readyCh)
	}
	r.mu.Unlock()
}

// Run seeds from the archive, then fetches until ctx is cancelled and the in-flight requests end. Each
// issuer has its own goroutine; a panic in one is recovered, reported and restarted with a backoff.
// Run returns nil on cancel. It may be called once at a time.
func (r *Refresher) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("audit: CRL refresher: already running")
	}
	r.running = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.running = false; r.mu.Unlock(); r.client.CloseIdleConnections() }()

	if err := r.Seed(ctx); err != nil && ctx.Err() == nil {
		r.emit(Event{Kind: EventArchiveError, Err: err, Detail: "seeding from the archive failed"})
	}
	var wg sync.WaitGroup
	for _, i := range r.issuers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			i.run(ctx)
		}()
	}
	wg.Wait()
	return nil
}

// run drives one issuer: the base feed and the delta feed in their own goroutines, the base one also
// watching the alarm.
func (i *issuerRT) run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, f := range []*feed{i.base, i.delta} {
		if f == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.loop(ctx)
		}()
	}
	wg.Wait()
}

// IssuerResult is the outcome of one issuer in RefreshOnce.
type IssuerResult struct {
	Issuer audit.SPKIHash
	Err    error
}

// RefreshOnce runs one attempt for every feed of every issuer (seeding from the archive first when the
// snapshot is empty) and returns each issuer's outcome: an error when no valid CRL was obtained or the
// newest one is not fresh. It is the cron form and the test hook.
func (r *Refresher) RefreshOnce(ctx context.Context) []IssuerResult {
	if r.appr.Status.Len() == 0 {
		if err := r.Seed(ctx); err != nil && ctx.Err() == nil {
			r.emit(Event{Kind: EventArchiveError, Err: err, Detail: "seeding from the archive failed"})
		}
	}
	var out []IssuerResult
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, i := range r.issuers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if i.delta != nil {
				// A delta outage is reported, not failed: the base CRL alone is a complete status, and
				// the freshness check below judges the merged view.
				if _, e := i.delta.attempt(ctx); e != nil {
					r.emit(Event{Kind: EventFetchFailed, Issuer: i.key, Feed: "delta", Err: e, Detail: "the delta feed failed; the pass is judged on the base and the freshness of what is held", At: r.now()})
				}
			}
			if _, e := i.base.attempt(ctx); e != nil {
				err = e
			}
			if err == nil {
				if h := i.health(r.now()); h.NewestThisUpdate.IsZero() || h.Critical {
					err = fmt.Errorf("audit: CRL refresher: issuer %x: no fresh CRL is held", i.key[:4])
				}
			}
			mu.Lock()
			out = append(out, IssuerResult{Issuer: i.key, Err: err})
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b IssuerResult) int { return cmpHash(a.Issuer, b.Issuer) })
	r.checkReady()
	return out
}

// Health returns the state of every issuer, ordered by key.
func (r *Refresher) Health() []IssuerHealth {
	now := r.now()
	out := make([]IssuerHealth, 0, len(r.issuers))
	for _, i := range r.issuers {
		out = append(out, i.health(now))
	}
	return out
}

// view is the newest base and, when paired, the newest delta the snapshot holds for the issuer, and the
// freshness figures the gate would use.
func (i *issuerRT) health(now time.Time) IssuerHealth {
	t := i.timing
	h := IssuerHealth{Issuer: i.key, URLs: redactAll(i.base.urls), DeltaURLs: redactAll(i.deltaURLs()), Limit: t.Limit}
	h.AlarmAge = t.alarmAge(t.interval(t.Period))
	var newestBase, newestDelta *audit.CRL
	for _, v := range i.r.appr.Status.HeldCRLs(i.cert) {
		if v.Delta {
			if newestDelta == nil || v.Number.Cmp(newestDelta.Number) > 0 {
				newestDelta = v
			}
		} else if newestBase == nil || v.Number.Cmp(newestBase.Number) > 0 {
			newestBase = v
		}
	}
	if newestBase != nil {
		h.NewestNumber, h.NewestThisUpdate, h.NewestNextUpdate = newestBase.Number, newestBase.ThisUpdate, newestBase.NextUpdate
	}
	if newestDelta != nil && newestBase != nil && newestDelta.Number.Cmp(newestBase.Number) > 0 &&
		newestDelta.BaseNumber.Cmp(newestBase.Number) <= 0 {
		h.Paired = true
		h.NewestNumber, h.NewestThisUpdate = newestDelta.Number, newestDelta.ThisUpdate
		h.NewestNextUpdate = minTime(newestBase.NextUpdate, newestDelta.NextUpdate)
	}
	if !h.NewestThisUpdate.IsZero() {
		h.Age = now.Sub(h.NewestThisUpdate)
		p := t.Period
		if h.Paired && i.delta != nil {
			p = i.delta.period
		}
		h.NextExpected = h.NewestThisUpdate.Add(t.Skew + p)
		h.Critical = t.critical(now, h.NewestThisUpdate, h.NewestNextUpdate)
		h.Alarm = h.Critical || h.Age > h.AlarmAge
	} else {
		h.Critical, h.Alarm = true, true
	}
	i.base.fill(&h)
	if i.delta != nil {
		i.delta.fillFailures(&h)
	}
	i.mu.Lock()
	h.Loop, h.UnackedResets = i.loop, i.unacked
	i.mu.Unlock()
	return h
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// checkAlarm evaluates the alarm and reports a change through the hook.
func (i *issuerRT) checkAlarm(now time.Time) {
	h := i.health(now)
	i.mu.Lock()
	changed := h.Alarm != i.alarm || h.Critical != i.critical
	i.alarm, i.critical = h.Alarm, h.Critical
	i.mu.Unlock()
	if !changed {
		return
	}
	d := "newest CRL age is within the alarm threshold"
	switch {
	case h.NewestThisUpdate.IsZero():
		d = "no CRL is held for this issuer"
	case h.Critical:
		d = "the newest CRL is no longer accepted by the gate: gates that depend on it are pausing"
	case h.Alarm:
		d = "the newest CRL is older than the alarm threshold: a CRL is late"
	}
	i.r.alarm(Alarm{Issuer: i.key, Active: h.Alarm, Critical: h.Critical, Age: h.Age, Threshold: h.AlarmAge, Limit: h.Limit, At: now, Detail: d})
}

func hexShort(h [32]byte) string { return hex.EncodeToString(h[:4]) }

func statusMax(s *audit.StatusSnapshot) int {
	_, _, m := s.Limits()
	return m
}

func mapKeys[K comparable, V any](m map[K]V) func(yield func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func cmpHash(a, b audit.SPKIHash) int { return slices.Compare(a[:], b[:]) }
