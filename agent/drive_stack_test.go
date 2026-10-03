//go:build !race

// The race detector's instrumentation changes frame sizes (and -race enables checkptr), so this
// measurement runs only in the plain build.

package agent

import (
	"context"
	"fmt"
	"runtime/debug"
	"testing"
	"unsafe"
)

// stackPattern marks the unused stack below the measuring frame.
const stackPattern = 0xABABABABABABABAB

// stackSpan is how far below its own frame measureStack paints (and so the most it can measure).
const stackSpan = 60 << 10

// growStack grows the calling goroutine's stack to hold about n KiB of frames, so the stack
// measureStack paints is already allocated and a measured call does not move it.
//
//go:noinline
func growStack(n int) byte {
	var buf [1 << 10]byte
	buf[n%len(buf)] = byte(n)
	if n > 0 {
		return growStack(n-1) + buf[n%len(buf)]
	}
	return buf[0]
}

// measureStack returns how many bytes of stack below its own frame f touches: it paints the
// unused stack below it with stackPattern, calls f, and finds the deepest word f changed. ok is
// false if the stack moved during f (it grew, and the runtime copied only the part in use), and
// the measurement is then void. The caller must have grown the stack past stackSpan (growStack)
// and must not let a GC shrink it meanwhile.
//
//go:noinline
func measureStack(f func()) (depth int, ok bool) {
	var top [8]byte
	topPtr := unsafe.Pointer(&top)
	base := unsafe.Add(topPtr, -stackSpan)
	// The 2 KiB just below top hold the rest of this frame; they are not painted.
	for off := 0; off < stackSpan-2<<10; off += 8 {
		*(*uint64)(unsafe.Add(base, off)) = stackPattern
	}
	f()
	if unsafe.Pointer(&top) != topPtr {
		return 0, false
	}
	for off := 0; off < stackSpan; off += 8 {
		if *(*uint64)(unsafe.Add(base, off)) != stackPattern {
			return stackSpan - off, true
		}
	}
	return 0, true
}

// TestDriveStackHighWater pins the stack a drive needs (runLoop, the body of Agent.run without
// probeDriveStack), which is what driveStackProbe is sized against. Below about 17 KiB the drive
// needs no more than the probe's 16 KiB frame plus a margin, so the probe would grow a stack the
// drive would not have grown (wasted work on every run in a fresh goroutine): drop the probe or
// shrink it. Above 32 KiB the drive grows its stack again past the probe, deep inside the drive,
// which the probe exists to avoid: resize the probe. Either way, re-measure the overhead
// benchmark (benchmarks/, bench.yml) before changing it.
func TestDriveStackHighWater(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // no GC: a GC may shrink the stack under the paint
	var turns []ScriptedTurn
	for i := range 9 {
		turns = append(turns, TextTurn(fmt.Sprint("answer ", i)))
	}
	a := mustNew(NewScriptedModel(turns...), memJournal())
	probe := 0 // the stack probeDriveStack touches: its frame, at least driveStackProbe bytes
	worst, valid := 0, 0
	for i := range 9 { // the first drive (one-time initialization in the process) is not counted
		done := make(chan struct{})
		go func() {
			defer close(done)
			growStack(stackSpan>>10 + 10)
			if d, ok := measureStack(func() { probeDriveStack() }); ok {
				probe = max(probe, d)
			}
			in := UserText("go")
			depth, ok := measureStack(func() {
				if _, _, _, err := a.runLoop(context.Background(), fmt.Sprint("hw-", i), &driveSpec{input: &in, strictSaga: true}); err != nil {
					t.Error(err)
				}
			})
			if ok && i > 0 {
				valid++
				worst = max(worst, depth)
			}
		}()
		<-done
	}
	if valid == 0 {
		t.Fatal("no valid measurement: the stack moved during every drive")
	}
	t.Logf("drive stack high-water mark: %d bytes (%d valid measurements); the probe's: %d", worst, valid, probe)
	if probe < driveStackProbe || probe > worst {
		t.Fatalf("probeDriveStack touches %d bytes of stack, want at least driveStackProbe (%d) and at most the drive's %d", probe, driveStackProbe, worst)
	}
	if lo, hi := 17<<10, 32<<10; worst < lo || worst > hi {
		t.Fatalf("drive stack high-water mark = %d bytes, want %d..%d: driveStackProbe (%d) is sized against it; see this test's comment",
			worst, lo, hi, driveStackProbe)
	}
}
