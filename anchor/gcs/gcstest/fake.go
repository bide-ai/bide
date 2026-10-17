// Package gcstest is an in-memory gcs.Store that models the parts of Cloud Storage the anchor relies
// on: object generations, create-only writes (ifGenerationMatch=0), a bucket retention policy that
// can be locked, noncurrent generations under versioning, temporary holds, per-object retention and
// Cloud KMS keys. It also has the means to do to the bucket what an attacker or an operator could
// (overwrite, delete the live object, tamper with bytes), and to inject faults.
//
// It models Cloud Storage as documented; it is not Cloud Storage. The opt-in test against a real
// bucket (BIDE_GCS_BUCKET) is what checks the model.
package gcstest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/anchor/gcs"
)

// ErrRetained is what a delete of a retained or held generation returns.
var ErrRetained = errors.New("gcstest: the generation is under retention or a hold")

type gen struct {
	info gcs.ObjectInfo
	body []byte
}

// Fake is the in-memory bucket. It is safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	clock   time.Time
	bucket  gcs.BucketInfo
	objs    map[string][]*gen // each name's generations, in increasing generation order
	nextGen int64
	calls   []string
	faults  []*fault
}

type fault struct {
	method string
	err    error
	times  int  // how many more calls fail
	after  bool // run the call, then fail (the response is lost)
}

// DefaultKMSKey is the default Cloud KMS key of a new Fake.
const DefaultKMSKey = "projects/p/locations/europe-west1/keyRings/r/cryptoKeys/anchor"

// ProjectNumber is the project number of a new Fake's bucket.
const ProjectNumber = 123456789012

// New returns a bucket that is what the anchor needs: a locked 30-day retention policy, versioning,
// uniform access, enforced public access prevention and DefaultKMSKey as its default key.
func New() *Fake {
	return &Fake{
		clock: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
		bucket: gcs.BucketInfo{
			ProjectNumber: ProjectNumber, RetentionPeriod: 30 * 24 * time.Hour, RetentionLocked: true,
			Versioning: true, DefaultKMSKey: DefaultKMSKey, UniformAccess: true, PublicAccessPrevented: true,
		},
		objs:    map[string][]*gen{},
		nextGen: 1_700_000_000_000_000,
	}
}

// Configure changes the bucket's configuration (to make a bucket the anchor must refuse).
func (f *Fake) Configure(fn func(*gcs.BucketInfo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.bucket)
}

// Now is the fake's clock. Advance moves it.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

// Advance moves the clock forward.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

// Calls returns the Store methods called so far, in order.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// FailNext makes the next times calls of method (Bucket, Create, Stat, Read or List) return err
// without running.
func (f *Fake) FailNext(method string, err error, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, &fault{method: method, err: err, times: times})
}

// LoseResponseNext makes the next call of method run and then return err: the effect happened and the
// caller does not know.
func (f *Fake) LoseResponseNext(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, &fault{method: method, err: err, times: 1, after: true})
}

// enter records a call and returns the fault to apply, if any. The caller holds the lock.
func (f *Fake) enter(method string) *fault {
	f.calls = append(f.calls, method)
	for i, ft := range f.faults {
		if ft.method == method && ft.times > 0 {
			ft.times--
			if ft.times == 0 {
				f.faults = slices.Delete(f.faults, i, i+1)
			}
			return ft
		}
	}
	return nil
}

func (f *Fake) live(name string) *gen {
	gs := f.objs[name]
	if n := len(gs); n > 0 && gs[n-1].info.Noncurrent.IsZero() {
		return gs[n-1]
	}
	return nil
}

func (f *Fake) find(name string, generation int64) *gen {
	if generation == 0 {
		return f.live(name)
	}
	for _, g := range f.objs[name] {
		if g.info.Generation == generation {
			return g
		}
	}
	return nil
}

// Bucket implements gcs.Store.
func (f *Fake) Bucket(context.Context) (gcs.BucketInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ft := f.enter("Bucket"); ft != nil && !ft.after {
		return gcs.BucketInfo{}, ft.err
	}
	return f.bucket, nil
}

// Create implements gcs.Store.
func (f *Fake) Create(_ context.Context, name string, body []byte, o gcs.CreateOptions) (gcs.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ft := f.enter("Create")
	if ft != nil && !ft.after {
		return gcs.ObjectInfo{}, ft.err
	}
	info, err := f.create(name, body, o)
	if ft != nil && ft.after {
		return gcs.ObjectInfo{}, ft.err
	}
	return info, err
}

func (f *Fake) create(name string, body []byte, o gcs.CreateOptions) (gcs.ObjectInfo, error) {
	if f.live(name) != nil {
		return gcs.ObjectInfo{}, fmt.Errorf("%w: %s (412 conditionNotMet)", gcs.ErrExists, name)
	}
	if o.Retention != nil && !f.bucket.ObjectRetention {
		return gcs.ObjectInfo{}, fmt.Errorf("gcstest: bucket does not have object retention enabled (400)")
	}
	key := o.KMSKey
	if key == "" {
		key = f.bucket.DefaultKMSKey
	}
	info := f.add(name, body, f.clock)
	if key != "" {
		info.KMSKey = key + "/cryptoKeyVersions/1"
	}
	info.TemporaryHold = o.TemporaryHold
	if o.Retention != nil {
		r := *o.Retention
		info.Retention = &r
	}
	f.objs[name][len(f.objs[name])-1].info = info
	return info, nil
}

// add appends a live generation, making the previous live one noncurrent, and returns its info.
func (f *Fake) add(name string, body []byte, at time.Time) gcs.ObjectInfo {
	if prev := f.live(name); prev != nil {
		prev.info.Noncurrent = at
	}
	f.nextGen++
	info := gcs.ObjectInfo{Name: name, Generation: f.nextGen, Size: int64(len(body)), Created: at, CRC32C: crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli))}
	if f.bucket.RetentionPeriod > 0 {
		info.RetentionExpiry = at.Add(f.bucket.RetentionPeriod)
	}
	f.objs[name] = append(f.objs[name], &gen{info: info, body: slices.Clone(body)})
	return info
}

// Stat implements gcs.Store.
func (f *Fake) Stat(_ context.Context, name string, generation int64) (gcs.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ft := f.enter("Stat"); ft != nil && !ft.after {
		return gcs.ObjectInfo{}, ft.err
	}
	g := f.find(name, generation)
	if g == nil {
		return gcs.ObjectInfo{}, fmt.Errorf("%w: %s generation %d (404)", gcs.ErrNoObject, name, generation)
	}
	return g.info, nil
}

// Read implements gcs.Store.
func (f *Fake) Read(_ context.Context, name string, generation, limit int64) ([]byte, gcs.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ft := f.enter("Read"); ft != nil && !ft.after {
		return nil, gcs.ObjectInfo{}, ft.err
	}
	g := f.find(name, generation)
	if g == nil {
		return nil, gcs.ObjectInfo{}, fmt.Errorf("%w: %s generation %d (404)", gcs.ErrNoObject, name, generation)
	}
	b := g.body
	if int64(len(b)) > limit+1 {
		b = b[:limit+1]
	}
	return slices.Clone(b), g.info, nil
}

// List implements gcs.Store.
func (f *Fake) List(_ context.Context, prefix string, versions bool, fn func(gcs.ObjectInfo) bool) error {
	f.mu.Lock()
	if ft := f.enter("List"); ft != nil && !ft.after {
		f.mu.Unlock()
		return ft.err
	}
	var infos []gcs.ObjectInfo
	names := make([]string, 0, len(f.objs))
	for n := range f.objs {
		if strings.HasPrefix(n, prefix) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		for _, g := range f.objs[n] {
			if versions || g.info.Noncurrent.IsZero() {
				infos = append(infos, g.info)
			}
		}
	}
	f.mu.Unlock()
	for _, i := range infos {
		if !fn(i) {
			return nil
		}
	}
	return nil
}

// The methods below are what an attacker, an operator or another tool can do to the bucket. They are
// not part of gcs.Store.

// Overwrite replaces the live object with other bytes (a new generation; the old one becomes
// noncurrent), as a principal with storage.objects.create and storage.objects.delete can.
func (f *Fake) Overwrite(name string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.add(name, body, f.clock)
	if k := f.bucket.DefaultKMSKey; k != "" {
		info.KMSKey = k + "/cryptoKeyVersions/1"
		f.objs[name][len(f.objs[name])-1].info = info
	}
}

// DeleteLive deletes the live generation of name. With versioning it becomes noncurrent and stays
// (the bucket-lock documentation: a live version with a future retention can still become noncurrent).
func (f *Fake) DeleteLive(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.live(name)
	if g == nil {
		return fmt.Errorf("%w: %s", gcs.ErrNoObject, name)
	}
	g.info.Noncurrent = f.clock
	return nil
}

// DeleteGeneration permanently deletes one generation. It fails with ErrRetained while the bucket's
// retention policy, a per-object retention or a hold covers it.
func (f *Fake) DeleteGeneration(name string, generation int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	gs := f.objs[name]
	for i, g := range gs {
		if g.info.Generation != generation {
			continue
		}
		if g.info.TemporaryHold || g.info.EventHold || g.info.RetentionExpiry.After(f.clock) ||
			(g.info.Retention != nil && g.info.Retention.RetainUntil.After(f.clock)) {
			return fmt.Errorf("%w: %s generation %d", ErrRetained, name, generation)
		}
		f.objs[name] = slices.Delete(gs, i, i+1)
		return nil
	}
	return fmt.Errorf("%w: %s generation %d", gcs.ErrNoObject, name, generation)
}

// ReleaseHold releases the temporary hold on a generation (what the separate hold-release identity does).
func (f *Fake) ReleaseHold(name string, generation int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.find(name, generation); g != nil {
		g.info.TemporaryHold = false
	}
}

// Tamper rewrites the bytes of a generation in place, which Cloud Storage does not allow; it models
// damage or a forged listing that the Reader must catch by checking the bytes.
func (f *Fake) Tamper(name string, generation int64, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.find(name, generation); g != nil {
		g.body = bytes.Clone(body)
		g.info.Size = int64(len(body))
	}
}

// Inject adds a generation exactly as given (a foreign writer's object: an unlocked one, a stray
// one, one under another key) and returns it. A zero Created is the fake's clock.
func (f *Fake) Inject(info gcs.ObjectInfo, body []byte) gcs.ObjectInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info.Created.IsZero() {
		info.Created = f.clock
	}
	if prev := f.live(info.Name); prev != nil && info.Noncurrent.IsZero() {
		prev.info.Noncurrent = info.Created
	}
	f.nextGen++
	info.Generation = f.nextGen
	info.Size = int64(len(body))
	info.CRC32C = crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli))
	f.objs[info.Name] = append(f.objs[info.Name], &gen{info: info, body: slices.Clone(body)})
	return info
}

// Names returns every object name, sorted.
func (f *Fake) Names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.objs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Generations returns the infos of every generation of name, oldest first.
func (f *Fake) Generations(name string) []gcs.ObjectInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gcs.ObjectInfo
	for _, g := range f.objs[name] {
		out = append(out, g.info)
	}
	return out
}

var _ gcs.Store = (*Fake)(nil)
