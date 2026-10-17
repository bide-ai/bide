package crlrefresh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/bide-ai/bide/audit"
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
func (i *issuerRT) verify(der []byte) (*audit.CRL, [32]byte, *verifyError) {
	hash := sha256.Sum256(der)
	_, maxEntries, _ := i.r.appr.Status.Limits()
	v, err := audit.VerifyCRL(der, i.cert, maxEntries)
	if err != nil {
		msg := err.Error()
		var se *audit.StatusError
		if errors.As(err, &se) {
			msg = se.Detail
		}
		if errors.Is(err, audit.ErrCRLSignature) {
			return nil, hash, &verifyError{EventBadSignature, strings.TrimPrefix(msg, audit.ErrCRLSignature.Error()+": ")}
		}
		return nil, hash, &verifyError{EventUnsupportedCRL, msg}
	}
	if d := i.r.appr.Policy.DistrustFrom(i.key); !d.IsZero() && !v.ThisUpdate.Before(d) {
		return nil, hash, &verifyError{EventDistrusted, "the CRL is signed by a key the policy distrusts from " + d.Format(time.RFC3339)}
	}
	return v, hash, nil
}

// cand is a CRL that passed verify, with where it came from.
type cand struct {
	v    *audit.CRL
	der  []byte
	tbs  [32]byte
	url  string
	hash [32]byte
	val  validator
	n    int // index of the URL in the feed, to report why its CRL was refused
}

// attempt asks every URL of the feed, concurrently and under one deadline, and adds the highest-numbered
// valid CRL (archiving every valid one). It returns whether a CRL was obtained and confirmed, and an
// error when none was.
func (f *feed) attempt(ctx context.Context) (bool, error) {
	f.attemptMu.Lock()
	defer f.attemptMu.Unlock()
	r := f.iss.r
	f.iss.syncResets(ctx, false)
	now := r.now()
	actx, cancel := context.WithTimeout(ctx, f.attemptTimeout)
	defer cancel()
	maxBytes, _, _ := r.appr.Status.Limits()

	res := make([]fetched, len(f.urls))
	// A small pool of workers takes the URLs from a queue filled and closed before they start, so no
	// goroutine holds a slot while it sends.
	jobs := make(chan int, len(f.urls))
	for n := range f.urls {
		jobs <- n
	}
	close(jobs)
	var wg sync.WaitGroup
	for range min(fetchConcurrency, len(f.urls)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range jobs {
				res[n] = r.fetchOne(actx, f.urls[n], maxBytes)
			}
		}()
	}
	wg.Wait()

	var cands []cand
	var outcomes []URLOutcome
	// refuse records on the URL's outcome that its CRL was fetched but not accepted, so a failed attempt
	// does not read "ok" for a URL whose CRL was refused.
	refuse := func(n int, kind, why string) {
		outcomes[n].Kind, outcomes[n].Detail = kind, why
	}
	var retry time.Duration
	confirmed := false
	for n, x := range res {
		outcomes = append(outcomes, x.URLOutcome)
		if x.Kind != outNotModified {
			r.setValidator(f.urls[n], validator{}) // set again only if this CRL is accepted
		}
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
				refuse(n, outRefused, verr.msg)
				continue
			}
			if v.Delta != (f.kind == "delta") {
				why := "a " + map[bool]string{true: "delta", false: "full"}[v.Delta] + " CRL was served at a " + f.kind + " URL"
				r.emit(Event{Kind: EventUnsupportedCRL, Issuer: f.iss.key, Feed: f.kind, URL: x.URL, Hash: hash, Number: v.Number, At: now, Detail: why})
				refuse(n, outRefused, why)
				continue
			}
			if len(v.DistributionPoints) > 0 && !slices.Contains(v.DistributionPoints, f.urls[n]) {
				r.emit(Event{Kind: EventUnsupportedCRL, Issuer: f.iss.key, Feed: f.kind, URL: x.URL, Hash: hash, Number: v.Number, At: now,
					Detail: "the CRL's issuing distribution point names " + strings.Join(v.DistributionPoints, ", ") + ", not the configured source " + f.urls[n] + ": the source is not the point the CA says the CRL is for"})
				continue
			}
			if lim := v.ThisUpdate.After(now.Add(f.iss.timing.Skew + time.Minute)); lim {
				f.iss.r.futureDated(f.iss, now)
				why := "thisUpdate " + v.ThisUpdate.Format(time.RFC3339) + " is ahead of the local clock beyond the declared skew and a minute"
				r.emit(Event{Kind: EventFutureDated, Issuer: f.iss.key, Feed: f.kind, URL: x.URL, Hash: hash, Number: v.Number, At: now, Detail: why})
				refuse(n, outRefused, why)
				continue
			}
			tbs, _ := audit.CRLTBSHash(x.der)
			cands = append(cands, cand{v: v, der: x.der, tbs: tbs, url: f.urls[n], hash: hash, val: x.val, n: n})
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
		if c := a.v.Number.Cmp(b.v.Number); c != 0 {
			return c
		}
		return strings.Compare(a.url, b.url)
	})
	obtained := confirmed
	for _, c := range cands {
		if ok, kind, why := f.consider(ctx, c, now, outcomes); ok {
			obtained = true
			r.setValidator(c.url, c.val)
		} else {
			refuse(c.n, kind, why)
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
// current one (newly accepted, or the one already held); when it is not, the second and third results
// are the URL outcome kind (outRefused, or outStale for a CRL that was added but is not fresh) and why.
func (f *feed) consider(ctx context.Context, c cand, now time.Time, outcomes []URLOutcome) (bool, string, string) {
	r := f.iss.r
	n := c.v.Number
	if c.v.Delta && !f.iss.basePaired(c.v) {
		// The delta names a base we do not hold: get the base first, then add the delta whether or not
		// the base arrived (a delta never gives evidence of absence alone, and its listings only take
		// trust away). This runs before the reset lock is taken: a base attempt takes it too.
		f.iss.base.attempt(ctx)
		if !f.iss.basePaired(c.v) {
			r.emit(Event{Kind: EventUnpairedDelta, Issuer: f.iss.key, Feed: f.kind, URL: audit.RedactURL(c.url), Number: n, Hash: c.hash, At: now,
				Detail: fmt.Sprintf("no base numbered %s to %s is held for delta %s", c.v.BaseNumber, new(big.Int).Sub(n, big.NewInt(1)), n)})
		}
	}
	// The set-aside check, the number rules, the add and the mark update are one step against a
	// concurrent reset (which takes the write side): otherwise a reset could remove the poisoned CRL
	// just before this adds it back.
	f.iss.resetMu.RLock()
	defer f.iss.resetMu.RUnlock()
	f.mu.Lock()
	mark, markTBS := f.mark, f.markTBS
	f.mu.Unlock()
	ev := func(k EventKind, detail string) Event {
		return Event{Kind: k, Issuer: f.iss.key, Feed: f.kind, URL: audit.RedactURL(c.url), Number: n, Held: mark, Hash: c.hash, Detail: detail, At: now}
	}
	refuse := func(k EventKind, detail string) (bool, string, string) {
		r.emit(ev(k, detail))
		return false, outRefused, detail
	}
	archived := func(marker string) ArchivedCRL {
		return ArchivedCRL{Number: n, ThisUpdate: c.v.ThisUpdate, Marker: marker, Source: audit.RedactURL(c.url), Fetched: now}
	}
	switch {
	case f.iss.setAside[hex.EncodeToString(c.hash[:])]:
		f.putArchive(ctx, archived(MarkerSetAside), c.der)
		return refuse(EventSetAsideServed, "the source serves a CRL an operator set aside (a high-water reset); not added")
	case mark != nil && n.Cmp(mark) < 0:
		f.putArchive(ctx, archived(MarkerRollback), c.der)
		return refuse(EventRollback, fmt.Sprintf("CRL number %s is below the high-water mark %s; not added", n, mark))
	case mark != nil && n.Cmp(mark) == 0:
		if c.tbs == markTBS {
			// The mark may come from a seed that stopped (a full snapshot, the scan cap, the budget)
			// before this CRL was added: make sure the gate holds it. AddCRL is idempotent.
			if !f.iss.holdsTBS(c.tbs) {
				if _, err := r.appr.Status.AddCRL(c.der); err != nil {
					return refuse(EventUnsupportedCRL, err.Error())
				}
			}
			r.emit(ev(EventUnchanged, "the newest CRL is unchanged"))
			return true, "", ""
		}
		f.putArchive(ctx, archived(MarkerConflict), c.der)
		return refuse(EventConflict, fmt.Sprintf("CRL number %s was seen with another TBSCertList; not added (the CA, or a source, is equivocating)", n))
	}
	f.putArchive(ctx, archived(""), c.der)
	if _, err := r.appr.Status.AddCRL(c.der); err != nil {
		return refuse(EventUnsupportedCRL, err.Error())
	}
	f.mu.Lock()
	f.mark, f.markTBS = n, c.tbs
	f.mu.Unlock()
	if !now.Before(c.v.NextUpdate) {
		const why = "the CRL was already past its nextUpdate when fetched; archived and added, not counted as fresh"
		r.emit(ev(EventStaleOnArrival, why))
		return false, outStale, why
	}
	e := ev(EventFetched, "CRL accepted")
	e.Outcomes = outcomes
	r.emit(e)
	return true, "", ""
}

// holdsTBS reports whether the snapshot holds a CRL of this issuer with the given TBSCertList (a re-signed
// copy is the same CRL).
func (i *issuerRT) holdsTBS(tbs [32]byte) bool {
	for _, v := range i.r.appr.Status.HeldCRLs(i.cert) {
		if v.TBSHash == tbs {
			return true
		}
	}
	return false
}

// basePaired reports whether the snapshot holds a full CRL the delta pairs with: numbered in
// [baseNumber, deltaNumber).
func (i *issuerRT) basePaired(d *audit.CRL) bool {
	for _, v := range i.r.appr.Status.HeldCRLs(i.cert) {
		if !v.Delta && v.Number.Cmp(d.BaseNumber) >= 0 && v.Number.Cmp(d.Number) < 0 {
			return true
		}
	}
	return false
}

// finish records the attempt in the schedule state and the health figures.
func (f *feed) finish(now time.Time, obtained bool, retry time.Duration, outcomes []URLOutcome) {
	have, this := false, time.Time{}
	var best *big.Int
	for _, v := range f.iss.r.appr.Status.HeldCRLs(f.iss.cert) {
		if v.Delta == (f.kind == "delta") && (best == nil || v.Number.Cmp(best) > 0) {
			best, this, have = v.Number, v.ThisUpdate, true
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
		p := o.URL + ": " + o.Kind
		if (o.Kind == outRefused || o.Kind == outStale) && o.Detail != "" {
			p += ": " + o.Detail
		}
		parts = append(parts, p)
	}
	f.lastErr = "no valid CRL obtained (" + strings.Join(parts, "; ") + ")"
}

// heldNext is the nextUpdate of the newest CRL of the feed's kind. Callers hold no feed lock needs: it
// reads only the snapshot.
func (f *feed) heldNext() time.Time {
	var best *audit.CRL
	for _, v := range f.iss.r.appr.Status.HeldCRLs(f.iss.cert) {
		if v.Delta == (f.kind == "delta") && (best == nil || v.Number.Cmp(best.Number) > 0) {
			best = v
		}
	}
	if best == nil {
		return time.Time{}
	}
	return best.NextUpdate
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
func (r *Refresher) futureDated(i *issuerRT, now time.Time) {
	r.mu.Lock()
	if r.futureAt == nil {
		r.futureAt = map[audit.SPKIHash]time.Time{}
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
func (r *Refresher) Seed(ctx context.Context) error {
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
	i.syncResets(ctx, true)
	for _, f := range []*feed{i.base, i.delta} {
		if f != nil {
			f.mu.Lock()
			f.mark, f.markTBS = nil, [32]byte{}
			f.mu.Unlock()
		}
	}
	if err := i.scan(ctx, i.setAsideCopy(), true); err != nil {
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
	for _, v := range f.iss.r.appr.Status.HeldCRLs(f.iss.cert) {
		if v.Delta == (f.kind == "delta") && (best == nil || v.Number.Cmp(best) > 0) {
			best, this, have = v.Number, v.ThisUpdate, true
		}
	}
	f.mu.Lock()
	f.st.have, f.st.this = have, this
	f.mu.Unlock()
}

func (i *issuerRT) setAsideCopy() map[string]bool {
	i.resetMu.RLock()
	defer i.resetMu.RUnlock()
	out := make(map[string]bool, len(i.setAside))
	for k := range i.setAside {
		out[k] = true
	}
	return out
}

// syncResets reads the issuer's RESET records. A record that appeared since the last read (written by
// ResetHighWater here, or by another process on a shared archive) takes effect: its CRLs are removed from
// the snapshot, its validators are dropped, and the marks are recomputed without them. It runs at the
// start of every attempt, so a reset from the command reaches a running daemon. With report, an
// unacknowledged record is reported (every start does).
func (i *issuerRT) syncResets(ctx context.Context, report bool, local ...bool) {
	r := i.r
	if r.archive == nil {
		return
	}
	resets, err := r.archive.Resets(ctx, i.key)
	if err != nil {
		r.emit(Event{Kind: EventArchiveError, Issuer: i.key, Err: err, Detail: "cannot read the reset records", At: r.now()})
		return
	}
	var names []string
	set := map[string]bool{}
	unacked := 0
	for _, rec := range resets {
		names = append(names, rec.Name)
		for _, it := range rec.SetAside {
			set[it.Hash] = true
		}
		if !rec.Acked {
			unacked++
			if report {
				r.emit(Event{Kind: EventHighWaterReset, Issuer: i.key, At: r.now(), Detail: fmt.Sprintf("unacknowledged high-water reset %s by %q at %s: %q; %d CRL(s) set aside", rec.Name, rec.Operator, rec.Time.Format(time.RFC3339), rec.Reason, len(rec.SetAside))})
			}
		}
	}
	key := strings.Join(names, ",")
	i.mu.Lock()
	i.unacked = unacked
	i.mu.Unlock()
	i.resetMu.Lock()
	defer i.resetMu.Unlock()
	switch {
	case !i.resetInit && (report || key == ""):
		// The first read: Seed computes the marks and fills the snapshot itself.
		i.resetInit, i.resetSeen, i.setAside = true, key, set
		return
	case i.resetInit && key == i.resetSeen:
		return
	}
	i.resetSeen, i.setAside, i.resetInit = key, set, true
	for h := range set {
		if raw, err := hex.DecodeString(h); err == nil && len(raw) == 32 {
			r.appr.Status.RemoveCRL([32]byte(raw))
		}
	}
	r.clearValidators(i.base.urls)
	if i.delta != nil {
		r.clearValidators(i.delta.urls)
	}
	for _, f := range []*feed{i.base, i.delta} {
		if f != nil {
			f.mu.Lock()
			f.mark, f.markTBS = nil, [32]byte{}
			f.mu.Unlock()
		}
	}
	if err := i.scan(ctx, set, false); err != nil {
		r.emit(Event{Kind: EventArchiveError, Issuer: i.key, Err: err, Detail: "cannot recompute the high-water mark after a reset", At: r.now()})
	}
	for _, f := range []*feed{i.base, i.delta} {
		if f != nil {
			f.finishSeed()
		}
	}
	if len(local) > 0 && local[0] {
		i.checkAlarm(r.now())
		return
	}
	r.emit(Event{Kind: EventHighWaterReset, Issuer: i.key, At: r.now(), Detail: fmt.Sprintf("a high-water reset is in force: %d CRL(s) set aside and removed from the live snapshot, the mark recomputed", len(set))})
	i.checkAlarm(r.now())
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
	added := 0
	for _, e := range list {
		if e.Marker != "" {
			continue
		}
		inWindow := now.Sub(e.ThisUpdate) <= window
		if (!inWindow || added >= i.need) && baseMark && deltaMark && wantLo == nil {
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
		if v.Delta {
			if i.delta == nil {
				continue
			}
			f = i.delta
		}
		tbs, _ := audit.CRLTBSHash(der)
		f.mu.Lock()
		if f.mark == nil || v.Number.Cmp(f.mark) > 0 {
			f.mark, f.markTBS = v.Number, tbs
		}
		f.mu.Unlock()
		if v.Delta {
			deltaMark = true
		} else {
			baseMark = true
		}
		if !intoSnapshot || full {
			continue
		}
		wanted := wantLo != nil && !v.Delta && v.Number.Cmp(wantLo) >= 0 && v.Number.Cmp(wantHi) < 0
		if (now.Sub(v.ThisUpdate) > window || added >= i.need) && !wanted {
			continue
		}
		if _, err := r.appr.Status.AddCRLNoEvict(der); err != nil {
			if errors.Is(err, audit.ErrSnapshotFull) {
				full = true
				r.emit(Event{Kind: EventSeedStopped, Issuer: i.key, Detail: "the snapshot is full; older CRLs were not loaded", At: now})
				continue
			}
			r.emit(Event{Kind: EventUnsupportedCRL, Issuer: i.key, Detail: e.Name + ": " + err.Error(), Hash: hash, At: now})
			continue
		}
		added++
		if wanted {
			wantLo, wantHi = nil, nil
		}
		if v.Delta && wantLo == nil && !i.basePairedHeld(v) {
			wantLo, wantHi = v.BaseNumber, v.Number
		}
	}
	return nil
}

func (i *issuerRT) basePairedHeld(d *audit.CRL) bool { return i.basePaired(d) }

// AckReset marks a high-water reset record of the issuer (by its name, as Health and the start-up event
// give it) as acknowledged by an operator: the start-up event for it stops.
func (r *Refresher) AckReset(ctx context.Context, issuer audit.SPKIHash, name string) error {
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

// ResetRequest is an operator's request to reset an issuer's high-water mark.
type ResetRequest struct {
	// Reason and Operator are required and recorded in the archive.
	Reason, Operator string
	// SetAside are the SHA-256 hashes of the archived CRLs to set aside. With none, the archived CRL at
	// the current mark.
	SetAside [][32]byte
	// AcceptLostListings allows the reset when a CRL set aside lists a certificate that no CRL left in the
	// snapshot lists: the reset would drop that revocation from the live gate until the CA publishes it
	// again. Without it such a reset is refused; with it the serials are written into the record.
	AcceptLostListings bool
}

// ResetHighWater is the operator's way out of a poisoned high-water mark: a CA that issued one CRL with a
// mistaken huge number, or was restored from a backup and restarted its numbering, would otherwise leave
// every later real CRL a rollback. It sets aside the CRLs named (or the archived CRL at the mark), appends
// a durable RESET record (issuer, what is set aside, the serials that no remaining CRL lists, reason,
// operator, time) to the archive, removes them from the snapshot and recomputes the mark from the rest.
// The set-aside files stay in the archive, and a source that keeps serving one has it archived with a
// marker and never added. A running refresher on a shared archive (another process) applies the record
// at its next attempt. It needs an archive, because the reset must survive a restart.
//
// A CRL set aside may be the newest real one. If it lists a revocation no remaining CRL lists, the reset
// would make status look better, so it is refused unless AcceptLostListings is set.
func (r *Refresher) ResetHighWater(ctx context.Context, issuer audit.SPKIHash, req ResetRequest) error {
	i := r.byKey[issuer]
	switch {
	case i == nil:
		return fmt.Errorf("audit: CRL refresher: %x is not an issuer of this refresher", issuer[:4])
	case r.archive == nil:
		return errors.New("audit: CRL refresher: a reset needs an archive: it must survive a restart")
	case strings.TrimSpace(req.Reason) == "" || strings.TrimSpace(req.Operator) == "":
		return errors.New("audit: CRL refresher: a reset needs a reason and an operator")
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
	// The CRLs to set aside, as parsed views.
	type aside struct {
		hash [32]byte
		num  *big.Int
		v    *audit.CRL
	}
	var set []aside
	load := func(e ArchivedCRL) (aside, bool) {
		der, err := r.archive.Read(ctx, issuer, e)
		if err != nil {
			return aside{}, false
		}
		h := sha256.Sum256(der)
		_, maxEntries, _ := r.appr.Status.Limits()
		v, err := audit.VerifyCRL(der, i.cert, maxEntries)
		if err != nil {
			return aside{hash: h, num: e.Number}, true
		}
		return aside{hash: h, num: e.Number, v: v}, true
	}
	if len(req.SetAside) > 0 {
		for _, h := range req.SetAside {
			found := false
			for _, e := range list {
				if a, ok := load(e); ok && a.hash == h {
					set, found = append(set, a), true
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
			if a, ok := load(e); ok {
				set = append(set, a)
			}
		}
		if len(set) == 0 {
			return errors.New("audit: CRL refresher: no archived CRL at the high-water mark to set aside; name the CRLs by hash")
		}
	}
	// The listings that would be lost: serials in the CRLs set aside that no remaining CRL of the snapshot lists.
	gone := map[[32]byte]bool{}
	for _, a := range set {
		gone[a.hash] = true
	}
	remaining := map[string]bool{}
	for _, v := range r.appr.Status.HeldCRLs(i.cert) {
		if !gone[v.Hash] {
			for _, k := range v.Serials() {
				remaining[k] = true
			}
		}
	}
	lostSet := map[string]bool{}
	for _, a := range set {
		if a.v != nil {
			for _, k := range a.v.Serials() {
				if !remaining[k] {
					lostSet[k] = true
				}
			}
		}
	}
	lost := make([]string, 0, len(lostSet))
	for k := range lostSet {
		lost = append(lost, k)
	}
	slices.Sort(lost)
	if len(lost) > 0 && !req.AcceptLostListings {
		return fmt.Errorf("audit: CRL refresher: setting these CRLs aside would drop %d revocation(s) no remaining CRL lists, so status would look better until the CA publishes them again; pass AcceptLostListings (bide identity reset-high-water -accept-lost-listings) to record that decision", len(lost))
	}
	now := r.now()
	rec := ResetRecord{Issuer: hex.EncodeToString(issuer[:]), Reason: req.Reason, Operator: req.Operator, Time: now, LostSerials: lost}
	for _, a := range set {
		rec.SetAside = append(rec.SetAside, ResetItem{Hash: hex.EncodeToString(a.hash[:]), Number: a.num.String()})
	}
	if err := r.archive.PutReset(ctx, issuer, rec); err != nil {
		return err
	}
	i.syncResets(ctx, false, true)
	old := "none"
	if mark != nil {
		old = mark.String()
	}
	r.emit(Event{Kind: EventHighWaterReset, Issuer: issuer, Held: mark, At: now,
		Detail: fmt.Sprintf("high-water mark reset by %q: %s (old mark %s, %d CRL(s) set aside and kept in the archive, %d listing(s) no remaining CRL carries)", req.Operator, req.Reason, old, len(rec.SetAside), len(lost))})
	return nil
}
