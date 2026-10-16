package audit

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	fetchConcurrency = 4
	alarmStep        = 30 * time.Second
	seedBudget       = 30 * time.Second
	seedScanCap      = 512
)

// feed is the base or the delta CRLs of one issuer: its URLs, its high-water mark and its schedule.
type feed struct {
	iss            *issuerRT
	kind           string // "base" or "delta"
	urls           []string
	period         time.Duration
	attemptTimeout time.Duration

	attemptMu sync.Mutex // one attempt at a time; a delta attempt may trigger a base attempt

	mu          sync.Mutex
	mark        *big.Int // the highest number accepted or seeded; nil before any
	markTBS     [32]byte
	st          feedState
	lastErr     string
	lastSuccess time.Time
	nextAttempt time.Time
	queue       []queuedCRL
	queueBytes  int
}

type queuedCRL struct {
	a   ArchivedCRL
	der []byte
}

// verifyError carries the event a refused CRL is reported as.
type verifyError struct {
	kind EventKind
	msg  string
}

func (e *verifyError) Error() string { return e.msg }

// verify applies the checks that precede anything touching the snapshot: the signature algorithm profile
// and the signature against THIS issuer's own certificate (not any issuer of the snapshot), the CRL's
// structure, the issuer name, and the policy's distrust date for the key.
func (i *issuerRT) verify(der []byte) (*crlView, [32]byte, *verifyError) {
	if err := authenticateWith(der, []*x509.Certificate{i.cert}); err != nil {
		var se *StatusError
		msg := err.Error()
		if errors.As(err, &se) {
			msg = se.Detail
		}
		return nil, [32]byte{}, &verifyError{EventBadSignature, msg}
	}
	hash := sha256.Sum256(der)
	v, err := parseCRL(der, hash, i.r.appr.Status.maxEntries)
	if err != nil {
		var se *StatusError
		msg := err.Error()
		if errors.As(err, &se) {
			msg = se.Detail
		}
		return nil, hash, &verifyError{EventUnsupportedCRL, msg}
	}
	if string(v.rl.RawIssuer) != string(i.cert.RawSubject) {
		return nil, hash, &verifyError{EventBadSignature, "the CRL's issuer name is not the issuing CA's subject"}
	}
	if d := i.r.appr.Policy.distrust[i.key]; !d.IsZero() && !v.this.Before(d) {
		return nil, hash, &verifyError{EventDistrusted, "the CRL is signed by a key the policy distrusts from " + d.Format(time.RFC3339)}
	}
	return v, hash, nil
}

// cand is a CRL that passed verify, with where it came from.
type cand struct {
	v    *crlView
	der  []byte
	tbs  [32]byte
	url  string
	hash [32]byte
}

// attempt asks every URL of the feed, concurrently and under one deadline, and adds the highest-numbered
// valid CRL (archiving every valid one). It returns whether a CRL was obtained and confirmed, and an
// error when none was.
func (f *feed) attempt(ctx context.Context) (bool, error) {
	f.attemptMu.Lock()
	defer f.attemptMu.Unlock()
	r := f.iss.r
	now := r.now()
	actx, cancel := context.WithTimeout(ctx, f.attemptTimeout)
	defer cancel()
	maxBytes := r.appr.Status.maxBytes

	res := make([]fetched, len(f.urls))
	sem := make(chan struct{}, fetchConcurrency)
	var wg sync.WaitGroup
	for n, u := range f.urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res[n] = r.fetchOne(actx, u, maxBytes)
		}()
	}
	wg.Wait()

	var cands []cand
	var outcomes []URLOutcome
	var retry time.Duration
	confirmed := false
	for n, x := range res {
		outcomes = append(outcomes, x.URLOutcome)
		retry = max(retry, x.retryAfter)
		if !x.date.IsZero() {
			if d := now.Sub(x.date); d > dateSkewWarnAt || -d > dateSkewWarnAt {
				r.emit(Event{Kind: EventClockSkew, Issuer: f.iss.key, Feed: f.kind, URL: x.URL, At: now,
					Detail: fmt.Sprintf("the server's Date header differs from the local clock by %v (advisory: never used for a decision)", d.Round(time.Second))})
			}
		}
		switch x.Kind {
		case outOK:
			v, hash, verr := f.iss.verify(x.der)
			if verr != nil {
				r.emit(Event{Kind: verr.kind, Issuer: f.iss.key, Feed: f.kind, URL: x.URL, Detail: verr.msg, Hash: hash, At: now})
				continue
			}
			if v.delta != (f.kind == "delta") {
				r.emit(Event{Kind: EventUnsupportedCRL, Issuer: f.iss.key, Feed: f.kind, URL: x.URL, Hash: hash, Number: v.number, At: now,
					Detail: "a " + map[bool]string{true: "delta", false: "full"}[v.delta] + " CRL was served at a " + f.kind + " URL"})
				continue
			}
			if lim := v.this.After(now.Add(f.iss.timing.Skew + time.Minute)); lim {
				f.iss.r.futureDated(f.iss, now)
				r.emit(Event{Kind: EventFutureDated, Issuer: f.iss.key, Feed: f.kind, URL: x.URL, Hash: hash, Number: v.number, At: now,
					Detail: "thisUpdate " + v.this.Format(time.RFC3339) + " is ahead of the local clock beyond the declared skew and a minute"})
				continue
			}
			tbs, _ := tbsHash(x.der)
			cands = append(cands, cand{v: v, der: x.der, tbs: tbs, url: f.urls[n], hash: hash})
		case outNotModified:
			confirmed = true
		case outOversize:
			r.emit(Event{Kind: EventOversizeResponse, Issuer: f.iss.key, Feed: f.kind, URL: x.URL, Detail: x.Detail, At: now})
		case outRedirect:
			r.emit(Event{Kind: EventRedirectRefused, Issuer: f.iss.key, Feed: f.kind, URL: x.URL, Detail: x.Detail, At: now})
		default:
			r.emit(Event{Kind: EventFetchFailed, Issuer: f.iss.key, Feed: f.kind, URL: x.URL, Detail: x.Kind + ": " + x.Detail, At: now})
		}
	}
	slices.SortStableFunc(cands, func(a, b cand) int {
		if c := a.v.number.Cmp(b.v.number); c != 0 {
			return c
		}
		return strings.Compare(a.url, b.url)
	})
	obtained := confirmed
	for _, c := range cands {
		if f.consider(ctx, c, now, outcomes) {
			obtained = true
		}
	}
	f.flushQueue(ctx)
	f.finish(now, obtained, retry, outcomes)
	f.iss.checkAlarm(r.now())
	r.checkReady()
	if !obtained {
		f.mu.Lock()
		msg := f.lastErr
		f.mu.Unlock()
		return false, fmt.Errorf("audit: CRL refresher: issuer %x (%s): %s", f.iss.key[:4], f.kind, msg)
	}
	return true, nil
}

// consider applies the number rules to one verified CRL. It reports whether the CRL is the issuer's
// current one (newly accepted, or the one already held).
func (f *feed) consider(ctx context.Context, c cand, now time.Time, outcomes []URLOutcome) bool {
	r := f.iss.r
	f.mu.Lock()
	mark, markTBS := f.mark, f.markTBS
	f.mu.Unlock()
	n := c.v.number
	ev := func(k EventKind, detail string) Event {
		return Event{Kind: k, Issuer: f.iss.key, Feed: f.kind, URL: redactURL(c.url), Number: n, Held: mark, Hash: c.hash, Detail: detail, At: now}
	}
	switch {
	case mark != nil && n.Cmp(mark) < 0:
		f.putArchive(ctx, ArchivedCRL{Number: n, ThisUpdate: c.v.this, Marker: MarkerRollback, Source: redactURL(c.url), Fetched: now}, c.der)
		r.emit(ev(EventRollback, fmt.Sprintf("CRL number %s is below the high-water mark %s; not added", n, mark)))
		return false
	case mark != nil && n.Cmp(mark) == 0:
		if c.tbs == markTBS {
			r.emit(ev(EventUnchanged, "the newest CRL is unchanged"))
			return true
		}
		f.putArchive(ctx, ArchivedCRL{Number: n, ThisUpdate: c.v.this, Marker: MarkerConflict, Source: redactURL(c.url), Fetched: now}, c.der)
		r.emit(ev(EventConflict, fmt.Sprintf("CRL number %s was seen with another TBSCertList; not added (the CA, or a source, is equivocating)", n)))
		return false
	}
	if c.v.delta && !f.iss.basePaired(c.v) {
		// The delta names a base we do not hold: get the base first, then add the delta whether or not
		// the base arrived (a delta never gives evidence of absence alone, and its listings only take
		// trust away).
		f.iss.base.attempt(ctx)
		if !f.iss.basePaired(c.v) {
			r.emit(ev(EventUnpairedDelta, fmt.Sprintf("no base numbered %s to %s is held for delta %s", c.v.baseNumber, new(big.Int).Sub(n, big.NewInt(1)), n)))
		}
	}
	f.putArchive(ctx, ArchivedCRL{Number: n, ThisUpdate: c.v.this, Source: redactURL(c.url), Fetched: now}, c.der)
	if _, err := r.appr.Status.AddCRL(c.der); err != nil {
		r.emit(ev(EventUnsupportedCRL, err.Error()))
		return false
	}
	f.mu.Lock()
	f.mark, f.markTBS = n, c.tbs
	f.mu.Unlock()
	if !now.Before(c.v.next) {
		r.emit(ev(EventStaleOnArrival, "the CRL was already past its nextUpdate when fetched; archived and added, not counted as fresh"))
		return false
	}
	e := ev(EventFetched, "CRL accepted")
	e.Outcomes = outcomes
	r.emit(e)
	return true
}

// basePaired reports whether the snapshot holds a full CRL the delta pairs with: numbered in
// [baseNumber, deltaNumber).
func (i *issuerRT) basePaired(d *crlView) bool {
	for _, v := range i.r.appr.Status.held(i.cert) {
		if !v.delta && v.number.Cmp(d.baseNumber) >= 0 && v.number.Cmp(d.number) < 0 {
			return true
		}
	}
	return false
}

// finish records the attempt in the schedule state and the health figures.
func (f *feed) finish(now time.Time, obtained bool, retry time.Duration, outcomes []URLOutcome) {
	have, this := false, time.Time{}
	var best *big.Int
	for _, v := range f.iss.r.appr.Status.held(f.iss.cert) {
		if v.delta == (f.kind == "delta") && (best == nil || v.number.Cmp(best) > 0) {
			best, this, have = v.number, v.this, true
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.lastAttempt, f.st.have, f.st.this, f.st.retryAfter = now, have, this, retry
	if obtained {
		f.st.failures, f.lastErr = 0, ""
		if have && f.iss.timing.Limit > 0 && !f.iss.timing.critical(now, this, f.heldNext()) {
			f.lastSuccess = now
		}
		return
	}
	f.st.failures++
	var parts []string
	for _, o := range outcomes {
		parts = append(parts, o.URL+": "+o.Kind)
	}
	f.lastErr = "no valid CRL obtained (" + strings.Join(parts, "; ") + ")"
}

// heldNext is the nextUpdate of the newest CRL of the feed's kind. Callers hold no feed lock needs: it
// reads only the snapshot.
func (f *feed) heldNext() time.Time {
	var best *crlView
	for _, v := range f.iss.r.appr.Status.held(f.iss.cert) {
		if v.delta == (f.kind == "delta") && (best == nil || v.number.Cmp(best.number) > 0) {
			best = v
		}
	}
	if best == nil {
		return time.Time{}
	}
	return best.next
}

// putArchive archives der, or queues it for retry when the archive refuses. The CRL is added to the
// snapshot regardless: withholding a CRL would make status look better.
func (f *feed) putArchive(ctx context.Context, a ArchivedCRL, der []byte) {
	r := f.iss.r
	if r.archive == nil {
		return
	}
	if err := r.archive.PutCRL(ctx, f.iss.key, a, der); err != nil {
		r.emit(Event{Kind: EventArchiveError, Issuer: f.iss.key, Feed: f.kind, Number: a.Number, Err: err, Detail: "the CRL could not be archived; it is queued for retry", At: r.now()})
		f.mu.Lock()
		f.queue = append(f.queue, queuedCRL{a, der})
		f.queueBytes += len(der)
		for f.queueBytes > r.maxQueue && len(f.queue) > 0 {
			old := f.queue[0]
			f.queue, f.queueBytes = f.queue[1:], f.queueBytes-len(old.der)
			r.emit(Event{Kind: EventArchiveDropped, Issuer: f.iss.key, Feed: f.kind, Number: old.a.Number, At: r.now(),
				Detail: "the archive retry queue is full: this CRL is not in the dated series"})
		}
		f.mu.Unlock()
	}
}

// flushQueue retries the CRLs the archive refused.
func (f *feed) flushQueue(ctx context.Context) {
	r := f.iss.r
	f.mu.Lock()
	q := f.queue
	f.queue, f.queueBytes = nil, 0
	f.mu.Unlock()
	var keep []queuedCRL
	for _, e := range q {
		if r.archive.PutCRL(ctx, f.iss.key, e.a, e.der) != nil {
			keep = append(keep, e)
		}
	}
	if len(keep) > 0 {
		f.mu.Lock()
		f.queue = append(keep, f.queue...)
		for _, e := range f.queue {
			f.queueBytes += len(e.der)
		}
		f.mu.Unlock()
	}
}

func (f *feed) fill(h *IssuerHealth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h.LastAttempt, h.LastSuccess, h.NextAttempt = f.st.lastAttempt, f.lastSuccess, f.nextAttempt
	h.ConsecutiveFailures, h.LastError, h.ArchiveBacklog = f.st.failures, f.lastErr, len(f.queue)
}

func (f *feed) fillFailures(h *IssuerHealth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h.ConsecutiveFailures = max(h.ConsecutiveFailures, f.st.failures)
	h.ArchiveBacklog += len(f.queue)
	if h.LastError == "" {
		h.LastError = f.lastErr
	}
}

// loop runs attempts until ctx ends. A panic is recovered, reported and the loop restarts with a backoff.
func (f *feed) loop(ctx context.Context) {
	r := f.iss.r
	restarts := 0
	for ctx.Err() == nil {
		if !f.guarded(ctx) {
			return
		}
		restarts++
		_ = r.sleep(ctx, backoff(restarts, f.iss.timing.interval(f.period), r.rnd(), 0))
	}
}

func (f *feed) guarded(ctx context.Context) (panicked bool) {
	r := f.iss.r
	defer func() {
		if x := recover(); x != nil {
			panicked = true
			r.emit(Event{Kind: EventPanic, Issuer: f.iss.key, Feed: f.kind, Detail: fmt.Sprint("recovered: ", x)})
		}
	}()
	for ctx.Err() == nil {
		_, _ = f.attempt(ctx)
		f.waitNext(ctx)
	}
	return false
}

// waitNext sleeps until the next attempt, in steps no longer than 30 seconds so the alarm is evaluated
// while the loop waits (a weekly root's loop sleeps for days).
func (f *feed) waitNext(ctx context.Context) {
	r := f.iss.r
	now := r.now()
	f.mu.Lock()
	st := f.st
	f.mu.Unlock()
	d := f.iss.timing.wait(st, f.period, now, r.rnd())
	end := now.Add(d)
	f.mu.Lock()
	f.nextAttempt = end
	f.mu.Unlock()
	for {
		now = r.now()
		f.iss.mu.Lock()
		f.iss.loop = now
		f.iss.mu.Unlock()
		if f.kind == "base" {
			f.iss.checkAlarm(now)
			r.checkReady()
		}
		rem := end.Sub(now)
		if rem <= 0 {
			return
		}
		if r.sleep(ctx, min(rem, alarmStep)) != nil {
			return
		}
	}
}

// futureDated notes a future-dated thisUpdate; several issuers doing it together point at a slow local
// clock, which is reported once as a warning.
func (r *CRLRefresher) futureDated(i *issuerRT, now time.Time) {
	r.mu.Lock()
	if r.futureAt == nil {
		r.futureAt = map[SPKIHash]time.Time{}
	}
	r.futureAt[i.key] = now
	n := 0
	for _, t := range r.futureAt {
		if now.Sub(t) < 10*time.Minute {
			n++
		}
	}
	warn := n >= 2 && now.Sub(r.skewWarned) > 10*time.Minute
	if warn {
		r.skewWarned = now
	}
	r.mu.Unlock()
	if warn {
		r.emit(Event{Kind: EventClockSkew, At: now, Detail: fmt.Sprintf("%d issuers show a future-dated thisUpdate: the local clock may be slow", n)})
	}
}

// Seed loads what the live rule needs from the archive into the snapshot, newest first, and takes the
// high-water marks from the CRLs that verify. A mark never comes from a file name alone: a stray or
// planted file with a huge number has no CRL behind it and is reported as corrupt. CRLs a reset record
// sets aside and CRLs marked rollback or conflict are never loaded. Seeding does not touch the
// snapshot's eviction bookkeeping, and stops when the snapshot is full.
func (r *CRLRefresher) Seed(ctx context.Context) error {
	if r.archive == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, seedBudget)
	defer cancel()
	for _, i := range r.issuers {
		if err := i.seed(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (i *issuerRT) seed(ctx context.Context) error {
	r := i.r
	setAside, err := i.setAside(ctx, true)
	if err != nil {
		return err
	}
	for _, f := range []*feed{i.base, i.delta} {
		if f != nil {
			f.mu.Lock()
			f.mark, f.markTBS = nil, [32]byte{}
			f.mu.Unlock()
		}
	}
	if err := i.scan(ctx, setAside, true); err != nil {
		return err
	}
	for _, f := range []*feed{i.base, i.delta} {
		if f != nil {
			f.finishSeed()
		}
	}
	r.checkReady()
	return nil
}

func (f *feed) finishSeed() {
	have, this := false, time.Time{}
	var best *big.Int
	for _, v := range f.iss.r.appr.Status.held(f.iss.cert) {
		if v.delta == (f.kind == "delta") && (best == nil || v.number.Cmp(best) > 0) {
			best, this, have = v.number, v.this, true
		}
	}
	f.mu.Lock()
	f.st.have, f.st.this = have, this
	f.mu.Unlock()
}

// setAside is the set of CRL hashes the issuer's reset records name. With report, an unacknowledged
// record is reported as a security event.
func (i *issuerRT) setAside(ctx context.Context, report bool) (map[string]bool, error) {
	set := map[string]bool{}
	resets, err := i.r.archive.Resets(ctx, i.key)
	if err != nil {
		return nil, err
	}
	unacked := 0
	for _, rec := range resets {
		for _, it := range rec.SetAside {
			set[it.Hash] = true
		}
		if !rec.Acked {
			unacked++
			if report {
				i.r.emit(Event{Kind: EventHighWaterReset, Issuer: i.key, Detail: fmt.Sprintf("unacknowledged high-water reset %s by %q at %s: %q; %d CRL(s) set aside", rec.Name, rec.Operator, rec.Time.Format(time.RFC3339), rec.Reason, len(rec.SetAside))})
			}
		}
	}
	i.mu.Lock()
	i.unacked = unacked
	i.mu.Unlock()
	return set, nil
}

// scan reads the archive newest number first. It takes marks from verified CRLs and, with intoSnapshot,
// seeds the snapshot with the CRLs within the status window (and the base of a seeded delta).
func (i *issuerRT) scan(ctx context.Context, setAside map[string]bool, intoSnapshot bool) error {
	r := i.r
	list, err := r.archive.List(ctx, i.key)
	if err != nil {
		return err
	}
	now := r.now()
	window := i.timing.Limit + i.timing.Skew + i.timing.Period
	scanned := 0
	baseMark, deltaMark := false, i.delta == nil
	var wantLo, wantHi *big.Int // the base range a seeded delta needs
	full := false
	for _, e := range list {
		if e.Marker != "" {
			continue
		}
		inWindow := now.Sub(e.ThisUpdate) <= window
		if !inWindow && baseMark && deltaMark && wantLo == nil {
			break
		}
		if scanned >= seedScanCap || ctx.Err() != nil {
			break
		}
		der, err := r.archive.Read(ctx, i.key, e)
		if err != nil {
			r.emit(Event{Kind: EventArchiveCorrupt, Issuer: i.key, Detail: "cannot read " + e.Name, Err: err, At: now})
			continue
		}
		hash := sha256.Sum256(der)
		if setAside[hex.EncodeToString(hash[:])] {
			continue
		}
		v, _, verr := i.verify(der)
		if verr != nil {
			r.emit(Event{Kind: EventArchiveCorrupt, Issuer: i.key, Detail: e.Name + " does not verify and is not used: " + verr.msg, Hash: hash, At: now})
			continue
		}
		scanned++
		f := i.base
		if v.delta {
			if i.delta == nil {
				continue
			}
			f = i.delta
		}
		tbs, _ := tbsHash(der)
		f.mu.Lock()
		if f.mark == nil || v.number.Cmp(f.mark) > 0 {
			f.mark, f.markTBS = v.number, tbs
		}
		f.mu.Unlock()
		if v.delta {
			deltaMark = true
		} else {
			baseMark = true
		}
		if !intoSnapshot || full {
			continue
		}
		wanted := wantLo != nil && !v.delta && v.number.Cmp(wantLo) >= 0 && v.number.Cmp(wantHi) < 0
		if now.Sub(v.this) > window && !wanted {
			continue
		}
		if _, err := r.appr.Status.addCRL(der, false); err != nil {
			if errors.Is(err, errSnapshotFull) {
				full = true
				r.emit(Event{Kind: EventSeedStopped, Issuer: i.key, Detail: "the snapshot is full; older CRLs were not loaded", At: now})
				continue
			}
			r.emit(Event{Kind: EventUnsupportedCRL, Issuer: i.key, Detail: e.Name + ": " + err.Error(), Hash: hash, At: now})
			continue
		}
		if wanted {
			wantLo, wantHi = nil, nil
		}
		if v.delta && wantLo == nil && !i.basePairedHeld(v) {
			wantLo, wantHi = v.baseNumber, v.number
		}
	}
	return nil
}

func (i *issuerRT) basePairedHeld(d *crlView) bool { return i.basePaired(d) }

// AckReset marks a high-water reset record of the issuer (by its name, as Health and the start-up event
// give it) as acknowledged by an operator: the start-up event for it stops.
func (r *CRLRefresher) AckReset(ctx context.Context, issuer SPKIHash, name string) error {
	if r.archive == nil {
		return errors.New("audit: CRL refresher: no archive")
	}
	resets, err := r.archive.Resets(ctx, issuer)
	if err != nil {
		return err
	}
	for _, rec := range resets {
		if rec.Name == name {
			return r.archive.AckReset(ctx, issuer, rec)
		}
	}
	return fmt.Errorf("audit: CRL refresher: no reset record %q", name)
}

// ResetHighWater is the operator's way out of a poisoned high-water mark: a CA that issued one CRL with a
// mistaken huge number, or was restored from a backup and restarted its numbering, would otherwise leave
// every later real CRL a rollback. It sets aside the CRLs given (hashes; with none, the archived CRL at
// the current mark), appends a durable RESET record (issuer, what is set aside, reason, operator, time)
// to the archive, removes them from the snapshot and recomputes the mark from the rest. The set-aside
// files stay in the archive. A listing on a CRL set aside no longer reaches the live gate: the record
// is the operator's statement that the CRL was wrong. It needs an archive, because the reset must
// survive a restart.
func (r *CRLRefresher) ResetHighWater(ctx context.Context, issuer SPKIHash, reason, operator string, setAside ...[32]byte) error {
	i := r.byKey[issuer]
	switch {
	case i == nil:
		return fmt.Errorf("audit: CRL refresher: %x is not an issuer of this refresher", issuer[:4])
	case r.archive == nil:
		return errors.New("audit: CRL refresher: a reset needs an archive: it must survive a restart")
	case strings.TrimSpace(reason) == "" || strings.TrimSpace(operator) == "":
		return errors.New("audit: CRL refresher: a reset needs a reason and an operator")
	}
	for _, f := range []*feed{i.base, i.delta} {
		if f != nil {
			f.attemptMu.Lock()
			defer f.attemptMu.Unlock()
		}
	}
	list, err := r.archive.List(ctx, issuer)
	if err != nil {
		return err
	}
	var mark *big.Int
	for _, f := range []*feed{i.base, i.delta} {
		if f != nil {
			f.mu.Lock()
			if f.mark != nil && (mark == nil || f.mark.Cmp(mark) > 0) {
				mark = f.mark
			}
			f.mu.Unlock()
		}
	}
	var items []ResetItem
	add := func(h [32]byte, n *big.Int) {
		items = append(items, ResetItem{Hash: hex.EncodeToString(h[:]), Number: n.String()})
	}
	if len(setAside) > 0 {
		for _, h := range setAside {
			found := false
			for _, e := range list {
				if der, err := r.archive.Read(ctx, issuer, e); err == nil && sha256.Sum256(der) == h {
					add(h, e.Number)
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("audit: CRL refresher: no archived CRL with hash %x", h[:4])
			}
		}
	} else {
		for _, e := range list {
			if e.Marker != "" || mark == nil || e.Number.Cmp(mark) != 0 {
				continue
			}
			if der, err := r.archive.Read(ctx, issuer, e); err == nil {
				add(sha256.Sum256(der), e.Number)
			}
		}
		if len(items) == 0 {
			return errors.New("audit: CRL refresher: no archived CRL at the high-water mark to set aside; name the CRLs by hash")
		}
	}
	now := r.now()
	rec := ResetRecord{Issuer: hex.EncodeToString(issuer[:]), SetAside: items, Reason: reason, Operator: operator, Time: now}
	if err := r.archive.PutReset(ctx, issuer, rec); err != nil {
		return err
	}
	set, err := i.setAside(ctx, false)
	if err != nil {
		return err
	}
	for _, it := range items {
		raw, _ := hex.DecodeString(it.Hash)
		r.appr.Status.remove([32]byte(raw))
	}
	for _, f := range []*feed{i.base, i.delta} {
		if f != nil {
			f.mu.Lock()
			f.mark, f.markTBS = nil, [32]byte{}
			f.mu.Unlock()
		}
	}
	if err := i.scan(ctx, set, false); err != nil {
		return err
	}
	for _, f := range []*feed{i.base, i.delta} {
		if f != nil {
			f.finishSeed()
		}
	}
	old := "none"
	if mark != nil {
		old = mark.String()
	}
	r.emit(Event{Kind: EventHighWaterReset, Issuer: issuer, Held: mark, At: now,
		Detail: fmt.Sprintf("high-water mark reset by %q: %s (old mark %s, %d CRL(s) set aside, kept in the archive)", operator, reason, old, len(items))})
	i.checkAlarm(now)
	return nil
}
