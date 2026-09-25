package agent

import (
	"context"
	"time"
)

// Leaser is the optional capability that coordinates driving a run across processes. Without it,
// several processes recovering against a shared store all re-drive the same in-flight runs: safe
// under at-most-once memoization but redundant, and a real hazard when the store's Do is not
// cross-process atomic (two live drivers could each run a non-memoized step before either records
// it). A store that implements Leaser lets a driver claim an exclusive, time-bounded lease on a run
// so only the holder drives it; a dead holder's lease expires and another process takes over, which
// is the high-availability property. Recover uses it automatically when the store provides it.
//
// The base Durable contract does not require leasing, and MemStore's implementation is in-process
// (for tests and as the reference); the cross-process payoff is a shared backend (store/postgres)
// implementing this with an atomic upsert over a leases table.
type Leaser interface {
	// AcquireLease claims runID for holder until now+ttl. It returns true if granted (the run is
	// unleased or the live lease is already holder's, which renews it), false if another holder
	// currently holds a live lease. An expired lease is available to any holder.
	AcquireLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error)
	// RenewLease extends holder's lease on runID, returning false if holder no longer holds it (it
	// expired or was taken). A driver whose work outlasts ttl renews to keep the lease.
	RenewLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error)
	// ReleaseLease relinquishes runID if held by holder (a no-op otherwise) so another process can
	// take it immediately rather than waiting for expiry.
	ReleaseLease(ctx context.Context, runID, holder string) error
}

// memLease is one in-memory lease: the current holder and when it expires.
type memLease struct {
	holder string
	expiry time.Time
}

// held reports whether the lease for runID is currently held by someone other than holder.
func (m *MemStore) heldByOther(runID, holder string, now time.Time) bool {
	cur, ok := m.leases[runID]
	return ok && cur.holder != holder && now.Before(cur.expiry)
}

// AcquireLease implements Leaser.
func (m *MemStore) AcquireLease(_ context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.heldByOther(runID, holder, now) {
		return false, nil
	}
	m.leases[runID] = memLease{holder: holder, expiry: now.Add(ttl)}
	return true, nil
}

// RenewLease implements Leaser.
func (m *MemStore) RenewLease(_ context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	cur, ok := m.leases[runID]
	if !ok || cur.holder != holder || !now.Before(cur.expiry) {
		return false, nil // no longer ours (expired or taken)
	}
	m.leases[runID] = memLease{holder: holder, expiry: now.Add(ttl)}
	return true, nil
}

// ReleaseLease implements Leaser.
func (m *MemStore) ReleaseLease(_ context.Context, runID, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.leases[runID]; ok && cur.holder == holder {
		delete(m.leases, runID)
	}
	return nil
}
