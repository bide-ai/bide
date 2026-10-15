package agent

import (
	"bytes"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
)

// This file is the test-only half of the trail locks (lockTrail): which goroutine holds how many, so
// a test can check that the goroutine about to call out to user code (the WithNotifyErrors handler)
// holds none. Tracking is off unless a test turns it on, and a process-wide count would be wrong: any
// other goroutine in the process may hold its own trail's lock at that moment.

// trackLockOwners turns on the tracking of which goroutine holds the trail locks.
var trackLockOwners atomic.Bool

var lockOwners = struct {
	mu   sync.Mutex
	held map[uint64]int
}{held: map[uint64]int{}}

// goroutineID is the calling goroutine's ID, read from its stack header ("goroutine 12 [running]:").
func goroutineID() uint64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	if i := bytes.IndexByte(b, ' '); i > 0 {
		b = b[:i]
	}
	id, _ := strconv.ParseUint(string(b), 10, 64)
	return id
}

// noteLockTaken records that the calling goroutine took a trail lock, and returns what releases it
// in the books (nil when tracking is off).
func noteLockTaken() func() {
	if !trackLockOwners.Load() {
		return nil
	}
	id := goroutineID()
	lockOwners.mu.Lock()
	lockOwners.held[id]++
	lockOwners.mu.Unlock()
	return func() {
		lockOwners.mu.Lock()
		if lockOwners.held[id]--; lockOwners.held[id] <= 0 {
			delete(lockOwners.held, id)
		}
		lockOwners.mu.Unlock()
	}
}

// trailLocksHeldByCaller is how many trail locks the calling goroutine holds (tracking must be on).
func trailLocksHeldByCaller() int {
	id := goroutineID()
	lockOwners.mu.Lock()
	defer lockOwners.mu.Unlock()
	return lockOwners.held[id]
}
