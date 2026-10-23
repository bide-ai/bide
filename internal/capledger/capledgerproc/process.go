// Package capledgerproc runs the cap ledger across real worker processes (re-executions of a store's
// test binary) for the tests of bide's stores. It is internal and starts a process, so only the store
// modules' tests import it (audit uses capledgertest, which starts none).
package capledgerproc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/capledger"
	"github.com/bide-ai/bide/internal/capledger/capledgertest"
)

// The multi-process check (C2 across processes, and C4 with a real crash): K worker processes, each a
// re-execution of the test binary, reserve against one window of one store. Each reserves the same S
// shared attempts first (a second driver of one attempt: they all start at one instant, so these race
// for the same slots) and then A attempts of its own, one unit each, against a limit smaller than the
// number of attempts. Every worker prints what it was told, and the parent checks the ledger: exactly
// the limit's worth of attempts holds a live slot, none holds two, the last slot's cum_after is the
// limit, the ledger has no gap (every read validates it), and what a worker was told agrees with the
// ledger. With Kill, one worker is SIGKILLed in the middle of its reservations and a replacement with
// the same name resumes them; the invariants must hold all the same, and a reconcile pass finds
// nothing to release.

const (
	workerEnv  = "BIDE_CAPLEDGER_WORKER"
	paramsEnv  = "BIDE_CAPLEDGER_PARAMS"
	startEnv   = "BIDE_CAPLEDGER_START"
	slowEnv    = "BIDE_CAPLEDGER_SLOW"
	releaseEnv = "BIDE_CAPLEDGER_RELEASE" // "1": the release phase, after every reservation is done
)

// Params are the numbers of a multi-process run.
type Params struct {
	Procs  int   // worker processes
	Own    int   // attempts each worker reserves of its own
	Shared int   // attempts every worker reserves, first
	Limit  int64 // the window's limit; each attempt reserves 1
	// Namespace names the rule's ledger, and prefixes the attempts, so a run of the test never meets
	// the slots or intents of an earlier one in a database that persists: the test gives each run its own.
	Namespace string
	// Release adds a phase after every reservation: each worker releases its first own attempt, then all
	// workers race to release the first shared attempt through Release and the second through a reconcile
	// pass; each shared slot must be released exactly once.
	Release bool
	// Kill SIGKILLs one worker in the middle of its reservations and starts a replacement of the same name.
	Kill bool
}

// String encodes the parameters for a worker's environment.
func (p Params) String() string {
	return fmt.Sprintf("%d,%d,%d,%d,%s", p.Procs, p.Own, p.Shared, p.Limit, p.Namespace)
}

func parseParams(s string) (p Params, err error) {
	f := strings.Split(s, ",")
	if len(f) != 5 {
		return p, fmt.Errorf("params %q", s)
	}
	p.Namespace = f[4]
	n := make([]int64, 4)
	for i := range n {
		if n[i], err = strconv.ParseInt(f[i], 10, 64); err != nil {
			return p, err
		}
	}
	p.Procs, p.Own, p.Shared, p.Limit = int(n[0]), int(n[1]), int(n[2]), n[3]
	return p, nil
}

var day = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func (p Params) rule() capledger.Rule {
	return capledger.Rule{Namespace: p.Namespace, Name: "total", Currency: "USD", Kind: capledger.Day}
}

func (p Params) request(tool string) capledger.Request {
	return capledger.Request{Rule: p.rule(), KeyDigest: capledger.KeyDigest(""), RunID: "run-" + tool, RootRunID: "root-" + p.Namespace,
		ToolUseID: tool, AttemptID: "1", Marked: true, Amount: 1, Limit: p.Limit}
}

// IsWorker reports whether this process is a worker; a store's TestMain calls WorkerMain then.
func IsWorker() bool { return os.Getenv(workerEnv) != "" }

// WorkerMain is a worker process: it opens its journal with open, waits for the start instant, reserves
// the shared attempts and then its own, printing one line per attempt ("ok <tool>" or "over <tool>"), and
// returns the exit code.
func WorkerMain(open func(ctx context.Context) (*agent.Journal, func(), error)) int {
	name := os.Getenv(workerEnv)
	p, err := parseParams(os.Getenv(paramsEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	j, closeFn, err := open(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		return 2
	}
	defer closeFn()
	l := capledgertest.New(j, func() time.Time { return day }, 0)
	var tools []string
	for i := range p.Shared {
		tools = append(tools, fmt.Sprintf("%s-shared-%d", p.Namespace, i))
	}
	for i := range p.Own {
		tools = append(tools, fmt.Sprintf("%s-%s-%d", p.Namespace, name, i))
	}
	if ns, err := strconv.ParseInt(os.Getenv(startEnv), 10, 64); err == nil {
		time.Sleep(time.Until(time.Unix(0, ns)))
	}
	slow, _ := time.ParseDuration(os.Getenv(slowEnv))
	if os.Getenv(releaseEnv) != "" {
		tools = nil // the release phase reserves nothing
	}
	for _, tool := range tools {
		_, err := l.Reserve(ctx, p.request(tool))
		switch {
		case err == nil:
			fmt.Println("ok", tool)
		case errors.Is(err, capledger.ErrOverLimit):
			fmt.Println("over", tool)
		default:
			fmt.Fprintln(os.Stderr, "worker:", tool, err)
			return 3
		}
		time.Sleep(slow)
	}
	if os.Getenv(releaseEnv) != "" {
		run := l.LedgerRun(p.rule(), "2026-10-22", capledger.KeyDigest(""))
		release := func(tool string) error {
			r := p.request(tool)
			var w atomic.Int32 // this worker's began word for the attempt: none, then sealed
			seal, ok := capledger.NewSeal(l.AttemptKey(r.RootRunID, r.ToolUseID, r.AttemptID), &w, 0, 2)
			if !ok {
				return nil
			}
			released, err := l.Release(ctx, capledger.Target{Rule: r.Rule, KeyDigest: r.KeyDigest, RunID: r.RunID, RootRunID: r.RootRunID,
				ToolUseID: r.ToolUseID, AttemptID: r.AttemptID}, seal)
			if err == nil && released {
				fmt.Println("released", tool)
			}
			return err
		}
		// Every worker releases its first own attempt, and all release the same shared slot at once through
		// Release (each with a seal of its own word: the point is the dedupe scan, not who may release) ...
		for _, tool := range []string{fmt.Sprintf("%s-%s-0", p.Namespace, name), fmt.Sprintf("%s-shared-0", p.Namespace)} {
			if err := release(tool); err != nil {
				fmt.Fprintln(os.Stderr, "worker: release:", err)
				return 3
			}
		}
		// ... and race a reconcile pass over another.
		shared := fmt.Sprintf("%s-shared-1", p.Namespace)
		got, err := l.Reconcile(ctx, run, func(_ context.Context, s capledger.Slot) (bool, error) { return s.Tool == shared, nil })
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker: reconcile:", err)
			return 3
		}
		for range got {
			fmt.Println("released", shared)
		}
	}
	return 0
}

// syncBuffer is a bytes.Buffer a process writes while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type worker struct {
	cmd    *exec.Cmd
	out    syncBuffer
	stderr syncBuffer
	err    error
	done   chan struct{}
}

func start(t *testing.T, p Params, name string, env []string, startAt time.Time, release bool) *worker {
	t.Helper()
	w := &worker{done: make(chan struct{})}
	w.cmd = exec.Command(os.Args[0], "-test.run=^$")
	w.cmd.Env = append(append(os.Environ(), env...), workerEnv+"="+name, paramsEnv+"="+p.String(),
		startEnv+"="+strconv.FormatInt(startAt.UnixNano(), 10))
	if p.Kill {
		w.cmd.Env = append(w.cmd.Env, slowEnv+"=30ms")
	}
	if release {
		w.cmd.Env = append(w.cmd.Env, releaseEnv+"=1")
	}
	w.cmd.Stdout, w.cmd.Stderr = &w.out, &w.stderr
	if err := w.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		w.err = w.cmd.Wait()
		close(w.done)
	}()
	return w
}

func (w *worker) lines() []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(w.out.String()))
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// Run starts p.Procs workers over the store that env (for the workers' open function) names, waits for
// them, and checks the ledger through j.
func Run(t *testing.T, j *agent.Journal, p Params, env []string) {
	t.Helper()
	startAt := time.Now().Add(1500 * time.Millisecond)
	var all []*worker // every process that ran, killed ones included
	cur := make([]*worker, p.Procs)
	for i := range cur {
		cur[i] = start(t, p, "w"+strconv.Itoa(i), env, startAt, false)
		all = append(all, cur[i])
	}
	t.Cleanup(func() {
		for _, w := range all {
			_ = w.cmd.Process.Kill()
		}
	})
	if p.Kill {
		// Kill worker 0 after it told the shared attempts and some of its own: a crash in the middle of
		// reservations, wherever the process then is (its intent written and no slot, a slot and no
		// cap-reserve record, between two). A replacement of the same name resumes.
		deadline := time.After(2 * time.Minute)
		for len(cur[0].lines()) < p.Shared+2 {
			select {
			case <-cur[0].done:
				t.Fatal("worker 0 finished before it could be killed")
			case <-deadline:
				t.Fatal("worker 0 did not start")
			case <-time.After(5 * time.Millisecond):
			}
		}
		_ = cur[0].cmd.Process.Kill()
		<-cur[0].done
		cur[0] = start(t, Params{Procs: p.Procs, Own: p.Own, Shared: p.Shared, Limit: p.Limit, Namespace: p.Namespace}, "w0", env, time.Now(), false)
		all = append(all, cur[0])
	}
	for i, w := range cur {
		select {
		case <-w.done:
			if w.err != nil {
				t.Fatalf("worker %d: %v: %s", i, w.err, w.stderr.String())
			}
		case <-time.After(3 * time.Minute):
			t.Fatalf("worker %d did not finish", i)
		}
	}
	if p.Release {
		// The release phase: every reservation is done, so the window is full; the workers release at one instant.
		at := time.Now().Add(1500 * time.Millisecond)
		var rel []*worker
		for i := range p.Procs {
			w := start(t, p, "w"+strconv.Itoa(i), env, at, true)
			rel = append(rel, w)
			all = append(all, w)
		}
		for i, w := range rel {
			select {
			case <-w.done:
				if w.err != nil {
					t.Fatalf("release worker %d: %v: %s", i, w.err, w.stderr.String())
				}
			case <-time.After(3 * time.Minute):
				t.Fatalf("release worker %d did not finish", i)
			}
		}
	}
	told := map[string]map[string]int{} // attempt -> "ok"/"over" -> count
	for _, w := range all {
		for _, line := range w.lines() {
			f := strings.Fields(line)
			if len(f) == 2 {
				if told[f[1]] == nil {
					told[f[1]] = map[string]int{}
				}
				told[f[1]][f[0]]++
			}
		}
	}
	l := capledgertest.New(j, func() time.Time { return day }, 0)
	runID := l.LedgerRun(p.rule(), "2026-10-22", capledger.KeyDigest(""))
	slots, err := l.Slots(context.Background(), runID) // validates the whole ledger
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	live, err := l.Live(context.Background(), runID)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	holds := map[string]bool{}
	for _, s := range live {
		if holds[s.Tool] {
			t.Fatalf("attempt %s holds two live slots", s.Tool)
		}
		holds[s.Tool] = true
	}
	released := map[string]int{}
	for _, w := range all {
		for _, line := range w.lines() {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "released" {
				released[f[1]]++
			}
		}
	}
	wantLive := p.Limit
	if p.Release {
		for a, n := range released {
			if n != 1 {
				t.Fatalf("attempt %s was released %d times, want once", a, n)
			}
			wantLive--
		}
		for _, k := range []string{"shared-0", "shared-1"} {
			if n := released[fmt.Sprintf("%s-%s", p.Namespace, k)]; n != 1 {
				t.Fatalf("%s was released %d times by %d racing workers, want once", k, n, p.Procs)
			}
		}
	}
	if int64(len(live)) != wantLive {
		t.Fatalf("%d live slots, want %d (limit %d, %d released, %d attempts asked)", len(live), wantLive, p.Limit, len(released), p.Procs*p.Own+p.Shared)
	}
	if last := slots[len(slots)-1]; !p.Release && last.CumAfter != p.Limit {
		t.Fatalf("the last slot's cum_after is %d, want the limit %d", last.CumAfter, p.Limit)
	} else if p.Release && last.CumAfter != wantLive {
		t.Fatalf("the last slot's cum_after is %d, want the live total %d", last.CumAfter, wantLive)
	}
	for a := range released {
		if holds[a] {
			t.Fatalf("attempt %s was released and still holds a live slot", a)
		}
	}
	for a, m := range told {
		if m["ok"] > 0 && !holds[a] && released[a] == 0 {
			t.Fatalf("attempt %s was told it holds a slot and the ledger has none", a)
		}
		if m["over"] > 0 && holds[a] {
			t.Fatalf("attempt %s was refused and the ledger holds a slot for it", a)
		}
	}
	if want := p.Procs*p.Own + p.Shared; len(told) != want {
		t.Fatalf("workers reported %d attempts, want %d", len(told), want)
	}
	// A reconcile pass over a ledger no call voided releases nothing, crash or not.
	rel, err := l.Reconcile(context.Background(), runID, func(context.Context, capledger.Slot) (bool, error) { return false, nil })
	if err != nil || len(rel) != 0 {
		t.Fatalf("Reconcile = %v, %v, want nothing released", rel, err)
	}
}
