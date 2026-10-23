package capledgertest

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
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/capledger"
)

// The multi-process check (C2 across processes): K worker processes, each a re-execution of the test
// binary, reserve against one window of one store. Each reserves A attempts of its own and the same S
// shared attempts (a second driver of one attempt), one unit each against a limit smaller than their
// number. Every worker prints what it was told, and the parent checks the ledger: exactly the limit's
// worth of attempts holds a live slot, none holds two, the ledger has no gap, and what a worker was told
// agrees with the ledger.

const (
	workerEnv = "BIDE_CAPLEDGER_WORKER"
	paramsEnv = "BIDE_CAPLEDGER_PARAMS"
)

// Params are the numbers of a multi-process run.
type Params struct {
	Procs  int   // worker processes
	Own    int   // attempts each worker reserves of its own
	Shared int   // attempts every worker reserves
	Limit  int64 // the window's limit; each attempt reserves 1
	// Namespace names the rule's ledger, so a run of the test never meets the slots of an earlier one
	// in a database that persists: the test gives each run its own.
	Namespace string
}

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

func (p Params) request(run, attempt string) capledger.Request {
	return capledger.Request{Rule: p.rule(), Limit: p.Limit, KeyDigest: capledger.KeyDigest(""), RunID: run, RootRunID: run, Attempt: attempt,
		Marked: true, Amount: 1}
}

// IsWorker reports whether this process is a worker; a store's TestMain calls WorkerMain then.
func IsWorker() bool { return os.Getenv(workerEnv) != "" }

// WorkerMain is a worker process: it opens its journal with open, reserves, prints one line per attempt
// ("ok <attempt>" or "over <attempt>") and returns the exit code.
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
	l := New(j, func() time.Time { return day }, 0)
	var attempts []string
	for i := range p.Own {
		attempts = append(attempts, fmt.Sprintf("%s-%d", name, i))
	}
	for i := range p.Shared {
		attempts = append(attempts, fmt.Sprintf("shared-%d", i))
	}
	for _, a := range attempts {
		_, err := l.Reserve(ctx, p.request("run-"+a, a))
		switch {
		case err == nil:
			fmt.Println("ok", a)
		case errors.Is(err, capledger.ErrOverLimit):
			fmt.Println("over", a)
		default:
			fmt.Fprintln(os.Stderr, "worker:", a, err)
			return 3
		}
	}
	return 0
}

// Run starts p.Procs workers over the store that env (for the workers' open function) names, waits for
// them, and checks the ledger through j.
func Run(t *testing.T, j *agent.Journal, p Params, env []string) {
	t.Helper()
	type result struct {
		out bytes.Buffer
		err error
	}
	results := make([]*result, p.Procs)
	cmds := make([]*exec.Cmd, p.Procs)
	var wg sync.WaitGroup
	for i := range cmds {
		results[i] = &result{}
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(append(os.Environ(), env...), workerEnv+"=w"+strconv.Itoa(i), paramsEnv+"="+p.String())
		cmd.Stdout = &results[i].out
		stderr := &bytes.Buffer{}
		cmd.Stderr = stderr
		cmds[i] = cmd
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := cmd.Wait(); err != nil {
				results[i].err = fmt.Errorf("%w: %s", err, stderr.String())
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Minute):
		for _, c := range cmds {
			_ = c.Process.Kill()
		}
		<-done
		t.Fatal("workers did not finish")
	}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("worker %d: %v", i, r.err)
		}
	}
	told := map[string]map[string]int{} // attempt -> "ok"/"over" -> count
	for _, r := range results {
		sc := bufio.NewScanner(&r.out)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 2 {
				if told[f[1]] == nil {
					told[f[1]] = map[string]int{}
				}
				told[f[1]][f[0]]++
			}
		}
	}
	// The ledger: read through a fresh ledger over the same journal.
	l := New(j, func() time.Time { return day }, 0)
	runID := l.LedgerRun(p.rule(), "2026-10-22", capledger.KeyDigest(""))
	live, err := l.Live(context.Background(), runID)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	holds := map[string]bool{}
	for _, s := range live {
		if holds[s.Attempt] {
			t.Fatalf("attempt %s holds two live slots", s.Attempt)
		}
		holds[s.Attempt] = true
	}
	if int64(len(live)) != p.Limit {
		t.Fatalf("%d live slots, want exactly the limit %d (%d attempts asked)", len(live), p.Limit, p.Procs*p.Own+p.Shared)
	}
	for a, m := range told {
		if m["ok"] > 0 && !holds[a] {
			t.Fatalf("attempt %s was told it holds a slot and the ledger has none", a)
		}
		if m["over"] > 0 && holds[a] {
			t.Fatalf("attempt %s was refused and the ledger holds a slot for it", a)
		}
	}
	if want := p.Procs*p.Own + p.Shared; len(told) != want {
		t.Fatalf("workers reported %d attempts, want %d", len(told), want)
	}
}
