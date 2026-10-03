//go:build unix

package postgres

// A multi-process high-availability harness: real OS processes (this test binary, re-executed as
// workers) drive the same runs through agent.RecoverLoop against one Postgres database, while the
// test kills, stalls and restarts them. Each run takes a non-retriable Step and two non-retriable
// tool calls, and every one of those side effects is an INSERT into bide_ha_effects, so the
// database itself counts how often each effect fired. Workers report what they did (entered an
// effect, halted, finished a drive) as rows in bide_ha_events, stamped by the database clock.
// Skips without PG_DSN.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime/pprof"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// Environment a worker process reads. haChildEnv holds the worker's name and marks the process as
// a worker; TestMain dispatches on it.
const (
	haChildEnv  = "BIDE_HA_CHILD"
	haHolderEnv = "BIDE_HA_HOLDER"
	haPrefixEnv = "BIDE_HA_PREFIX"
	haTTLEnv    = "BIDE_HA_TTL"
)

// How long an effect takes around its INSERT: the windows in which the test kills or stalls a
// worker, before the effect fires and after it fired but before the result is journaled.
const (
	haPreEffect  = 300 * time.Millisecond
	haPostEffect = 400 * time.Millisecond
)

// haRecoverInterval is how often a worker's RecoverLoop starts a pass.
const haRecoverInterval = 50 * time.Millisecond

// haLoad is the factor by which the test lets the machine run slower than the timings the workers
// are configured with (effect windows, lease TTL, recover interval) before a wait fails: the
// race detector and a shared CI runner slow every process and every database round trip.
const haLoad = 4

// takeoverBound is how long after a worker stops (killed or stalled) another driver may take to
// halt on its claim. The stopped worker's lease was last renewed before it stopped, so it lapses
// at most ttl after the stop. A worker drives one run at a time, so a survivor may first finish
// the run it is driving (three effects), and then starts a pass within haRecoverInterval. Each
// cluster has a schema of its own, so a pass reads only the cluster's few runs: its length does
// not depend on what other tests left in the database.
func (c *haCluster) takeoverBound() time.Duration {
	return haLoad * (c.ttl + haRecoverInterval + 3*(haPreEffect+haPostEffect))
}

// Exit codes a worker uses to report a failure the parent must see.
const (
	haExitSetup = 2 // could not open the store or read its configuration
	haExitLeak  = 3 // a lease renewer outlived every drive at shutdown
)

func TestMain(m *testing.M) {
	if name := os.Getenv(haChildEnv); name != "" {
		os.Exit(haWorker(name))
	}
	os.Exit(m.Run())
}

const haSchema = `
	CREATE TABLE IF NOT EXISTS bide_ha_effects (
		run_id text        NOT NULL,
		effect text        NOT NULL,
		worker text        NOT NULL,
		at     timestamptz NOT NULL DEFAULT clock_timestamp()
	);
	CREATE TABLE IF NOT EXISTS bide_ha_events (
		run_id text        NOT NULL,
		effect text        NOT NULL,
		worker text        NOT NULL,
		phase  text        NOT NULL,
		at     timestamptz NOT NULL DEFAULT clock_timestamp()
	);`

// haWorker is a worker process: it runs agent.RecoverLoop until SIGTERM, driving the runs whose
// IDs carry its prefix, then checks that no lease renewer is left running.
func haWorker(name string) int {
	holder, prefix := os.Getenv(haHolderEnv), os.Getenv(haPrefixEnv)
	ttl, err := time.ParseDuration(os.Getenv(haTTLEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		return haExitSetup
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // never outlive a parent that died
	defer cancel()

	s, err := Open(ctx, os.Getenv("PG_DSN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		return haExitSetup
	}
	j := agenttest.MustJournal(s)
	defer s.Close()
	w := &haDriver{s: s, worker: name, prefix: prefix}

	// One run at a time, so a killed worker leaves exactly one call in flight for the test to follow.
	err = agent.RecoverLoop(ctx, j, w.resume, agent.WithLeaseHolder(holder), agent.WithLeaseTTL(ttl),
		agent.WithRecoverInterval(haRecoverInterval), agent.WithRecoverConcurrency(1),
		agent.WithRecoverErrors(func(err error) { fmt.Fprintf(os.Stderr, "worker %s: recover: %v\n", name, err) }))
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "worker %s: RecoverLoop: %v\n", name, err)
		return haExitSetup
	}

	// RecoverLoop waited for its drives, so every renewer must have exited with them.
	var stacks bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&stacks, 1)
	if strings.Contains(stacks.String(), "driveWithRenew") {
		fmt.Fprintf(os.Stderr, "worker %s: lease renewer still running after every drive returned:\n%s\n", name, stacks.String())
		return haExitLeak
	}
	return 0
}

// haDriver drives one worker's runs: a non-retriable Step, then an agent whose model calls a
// non-retriable tool twice before answering.
type haDriver struct {
	s              *Store
	worker, prefix string
}

func (w *haDriver) resume(ctx context.Context, runID string, _ agent.RunStart) error {
	if !strings.HasPrefix(runID, w.prefix) {
		return nil // another test's run: not ours to drive
	}
	w.event(runID, "", "resume")
	err := w.drive(ctx, runID)
	var halt *agent.OutcomeUnknown
	switch {
	case errors.Is(context.Cause(ctx), agent.ErrLeaseLost):
		w.event(runID, "", "lost")
	case errors.As(err, &halt):
		w.event(runID, halt.Op.ID, "halt")
	case err != nil:
		w.event(runID, "", "error")
	default:
		w.event(runID, "", "done")
	}
	return err
}

func (w *haDriver) drive(ctx context.Context, runID string) error {
	if _, err := agenttest.MustJournal(w.s).Step(ctx, runID, "reserve", func(context.Context) (string, error) {
		return w.effect(runID, "reserve"), nil
	}); err != nil {
		return err
	}
	charge := agent.Func("charge", "charge the card", agent.Safety{},
		func(_ context.Context, in struct{ Key string }) (string, error) {
			return w.effect(runID, in.Key), nil
		})
	model := agent.NewScriptedModel(
		agent.ToolTurn("c1", "charge", `{"Key":"c1"}`),
		agent.ToolTurn("c2", "charge", `{"Key":"c2"}`),
		agent.TextTurn("done"),
	)
	_, err := agenttest.MustNew(model, agenttest.MustJournal(w.s), agent.WithTools(charge)).Run(ctx, runID, agent.UserText("go"))
	return err
}

// effect is the side effect: it announces itself, waits, fires (one INSERT, whatever the driver's
// context says, as a real downstream call would complete), and waits again before returning, so
// the test can land a signal before or after the effect fires.
func (w *haDriver) effect(runID, key string) string {
	w.event(runID, key, "enter")
	time.Sleep(haPreEffect)
	if _, err := w.s.db.ExecContext(context.Background(),
		`INSERT INTO bide_ha_effects (run_id, effect, worker) VALUES ($1, $2, $3)`, runID, key, w.worker); err != nil {
		fmt.Fprintf(os.Stderr, "worker %s: effect: %v\n", w.worker, err)
	}
	w.event(runID, key, "fired")
	time.Sleep(haPostEffect)
	return "ok"
}

func (w *haDriver) event(runID, effect, phase string) {
	if _, err := w.s.db.ExecContext(context.Background(),
		`INSERT INTO bide_ha_events (run_id, effect, worker, phase) VALUES ($1, $2, $3, $4)`, runID, effect, w.worker, phase); err != nil {
		fmt.Fprintf(os.Stderr, "worker %s: event: %v\n", w.worker, err)
	}
}

// haCluster is the parent's view of the worker processes and the shared database.
type haCluster struct {
	t      *testing.T
	s      *Store
	db     *sql.DB
	dsn    string // PG_DSN with the cluster's own schema on its search_path
	prefix string
	ttl    time.Duration
	runs   []string

	mu      sync.Mutex
	workers map[string]*haProc
}

type haProc struct {
	cmd    *exec.Cmd
	out    *syncBuffer
	exited chan struct{}
	killed bool // SIGKILLed by the test, so its exit status is expected
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// newHACluster creates the cluster's runs in a schema of its own, which its workers open too. A
// worker's RecoverLoop lists every incomplete run in the store and leases each in turn, one at a
// time, so over the shared schema a pass would also walk every run other tests left unfinished
// (halted runs, runs no one drives): thousands after one pass of the package, and the pass, and so
// the time to take over a stopped worker's run, would grow with them rather than with the TTL.
func newHACluster(t *testing.T, runs int, ttl time.Duration) *haCluster {
	t.Helper()
	dsn, _, _ := freshSchema(t)
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(s)
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.ExecContext(ctx, haSchema); err != nil {
		t.Fatal(err)
	}
	c := &haCluster{t: t, s: s, db: s.db, dsn: dsn, prefix: strings.ReplaceAll(uniqueID(t, "ha-"), "/", "-") + "-", ttl: ttl, workers: map[string]*haProc{}}
	for i := range runs {
		id := fmt.Sprintf("%srun%d", c.prefix, i)
		// A run:start makes the run exist and started (its input, the one every worker drives it
		// with), so Recover finds and drives it; no primary ever drives it.
		if _, err := journaltest.Do(ctx, j, id, "run:start", func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: []byte(`{"input":"go"}`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
		c.runs = append(c.runs, id)
	}
	t.Cleanup(c.shutdown)
	return c
}

// start launches a worker process named name that claims leases as holder.
func (c *haCluster) start(name, holder string) {
	c.t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "PG_DSN="+c.dsn, // the last PG_DSN in Env is the one the worker sees
		haChildEnv+"="+name, haHolderEnv+"="+holder, haPrefixEnv+"="+c.prefix, haTTLEnv+"="+c.ttl.String())
	p := &haProc{cmd: cmd, out: &syncBuffer{}, exited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.out, p.out
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	go func() { _ = cmd.Wait(); close(p.exited) }()
	c.mu.Lock()
	c.workers[name] = p
	c.mu.Unlock()
}

func (c *haCluster) signal(name string, sig syscall.Signal) {
	c.t.Helper()
	c.mu.Lock()
	p := c.workers[name]
	if sig == syscall.SIGKILL {
		p.killed = true
	}
	c.mu.Unlock()
	if err := p.cmd.Process.Signal(sig); err != nil {
		c.t.Fatalf("signal %v to %s: %v", sig, name, err)
	}
	if sig == syscall.SIGKILL {
		<-p.exited
	}
}

// shutdown stops every worker with SIGTERM and fails the test if one that was not killed on
// purpose exited with an error: a race report, a leaked renewer, or a setup failure.
func (c *haCluster) shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.workers {
		if !p.killed {
			_ = p.cmd.Process.Signal(syscall.SIGCONT) // a stalled worker must wake to exit
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	for name, p := range c.workers {
		select {
		case <-p.exited:
		case <-time.After(30 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.exited
			c.t.Errorf("worker %s did not exit on SIGTERM\n%s", name, p.out.String())
			continue
		}
		if !p.killed && !p.cmd.ProcessState.Success() {
			c.t.Errorf("worker %s exited with %v\n%s", name, p.cmd.ProcessState, p.out.String())
		}
	}
}

// dbNow reads the database clock, the one the events are stamped with.
func (c *haCluster) dbNow() time.Time {
	c.t.Helper()
	var now time.Time
	if err := c.db.QueryRow(`SELECT clock_timestamp()`).Scan(&now); err != nil {
		c.t.Fatal(err)
	}
	return now
}

type haEvent struct {
	run, effect, worker, phase string
	at                         time.Time
}

// waitEvent polls until an event matching match is recorded, and returns it.
func (c *haCluster) waitEvent(what string, timeout time.Duration, match func(haEvent) bool) haEvent {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range c.events() {
			if match(e) {
				return e
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("timed out after %v waiting for %s\n%s", timeout, what, c.dump())
	return haEvent{}
}

func (c *haCluster) events() []haEvent {
	c.t.Helper()
	rows, err := c.db.Query(`SELECT run_id, effect, worker, phase, at FROM bide_ha_events WHERE run_id LIKE $1 || '%' ORDER BY at`, c.prefix)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	var out []haEvent
	for rows.Next() {
		var e haEvent
		if err := rows.Scan(&e.run, &e.effect, &e.worker, &e.phase, &e.at); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// effects returns, per run, how many times each effect fired and which workers fired it.
func (c *haCluster) effects() map[string]map[string][]string {
	c.t.Helper()
	rows, err := c.db.Query(`SELECT run_id, effect, worker FROM bide_ha_effects WHERE run_id LIKE $1 || '%'`, c.prefix)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]map[string][]string{}
	for rows.Next() {
		var run, effect, worker string
		if err := rows.Scan(&run, &effect, &worker); err != nil {
			c.t.Fatal(err)
		}
		if out[run] == nil {
			out[run] = map[string][]string{}
		}
		out[run][effect] = append(out[run][effect], worker)
	}
	return out
}

// waitComplete waits until every run in want has its completion marker.
func (c *haCluster) waitComplete(want []string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var pending []string
		for _, id := range want {
			done, err := agent.IsComplete(context.Background(), agenttest.MustJournal(c.s), id)
			if err != nil {
				c.t.Fatal(err)
			}
			if !done {
				pending = append(pending, id)
			}
		}
		if len(pending) == 0 {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("runs never completed after %v: %v\n%s", timeout, pending, c.dump())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// checkAtMostOnce fails the test if any effect fired more than once, and returns the counts.
func (c *haCluster) checkAtMostOnce() map[string]map[string][]string {
	c.t.Helper()
	fx := c.effects()
	for run, byEffect := range fx {
		for effect, workers := range byEffect {
			if len(workers) > 1 {
				c.t.Errorf("run %s: effect %s fired %d times (by %v), want at most once", run, effect, len(workers), workers)
			}
		}
	}
	return fx
}

// checkCompleteRuns requires every effect of each completed run to have fired exactly once.
func (c *haCluster) checkCompleteRuns(fx map[string]map[string][]string, runs []string) {
	c.t.Helper()
	for _, run := range runs {
		for _, effect := range []string{"reserve", "c1", "c2"} {
			if n := len(fx[run][effect]); n != 1 {
				c.t.Errorf("completed run %s: effect %s fired %d times, want exactly 1", run, effect, n)
			}
		}
	}
}

// checkRecoverSkipsCompleted lets the workers make several more Recover passes and requires that
// none of them re-drove a completed run.
func (c *haCluster) checkRecoverSkipsCompleted(runs []string) {
	c.t.Helper()
	since := c.dbNow()
	time.Sleep(500 * time.Millisecond) // about ten Recover passes per worker
	complete := map[string]bool{}
	for _, r := range runs {
		complete[r] = true
	}
	for _, e := range c.events() {
		if e.at.After(since) && complete[e.run] && e.phase == "resume" {
			c.t.Errorf("worker %s re-drove run %s after it completed", e.worker, e.run)
		}
	}
}

func (c *haCluster) dump() string {
	var b strings.Builder
	b.WriteString("events:\n")
	for _, e := range c.events() {
		fmt.Fprintf(&b, "  %s %-6s %-12s %-8s %s\n", e.at.Format("15:04:05.000"), e.phase, e.worker, e.effect, strings.TrimPrefix(e.run, c.prefix))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, p := range c.workers {
		if out := p.out.String(); out != "" {
			fmt.Fprintf(&b, "worker %s output:\n%s\n", name, out)
		}
	}
	return b.String()
}

func without(runs []string, drop string) []string {
	var out []string
	for _, r := range runs {
		if r != drop {
			out = append(out, r)
		}
	}
	return out
}

// Workers are SIGKILLed with a non-retriable tool call in flight, one before its effect fires and
// one after the effect fired but before its result is journaled, and each is restarted under the
// same holder name. The dead worker's lease expires and a survivor takes the run over, finds the
// call's attempt marker with no result, and halts with ResumeHalt instead of firing the effect: its
// outcome is unknown to the journal, so a human must resolve it. Every other run completes with
// each effect fired exactly once.
func TestHA_MultiProcessKillAndRestart(t *testing.T) {
	c := newHACluster(t, 8, time.Second)
	for _, w := range []string{"w0", "w1", "w2"} {
		c.start(w, w)
	}

	// Before the effect: killed as the first tool call starts.
	before := c.waitEvent("a worker to enter the first tool call", 30*time.Second, func(e haEvent) bool {
		return e.phase == "enter" && e.effect == "c1"
	})
	c.signal(before.worker, syscall.SIGKILL)
	killedBefore := c.dbNow()
	c.start(before.worker+"-restarted", before.worker)

	// After the effect: another worker, killed once its second tool call has fired.
	after := c.waitEvent("another worker to fire a second tool call", 30*time.Second, func(e haEvent) bool {
		return e.phase == "fired" && e.effect == "c2" && e.run != before.run && e.worker != before.worker && e.at.After(killedBefore)
	})
	c.signal(after.worker, syscall.SIGKILL)
	killedAfter := c.dbNow()
	c.start(after.worker+"-restarted", after.worker)

	survivors := without(without(c.runs, before.run), after.run)
	c.waitComplete(survivors, 40*time.Second)
	for _, k := range []struct {
		victim   haEvent
		killedAt time.Time
		fired    int
	}{{before, killedBefore, 0}, {after, killedAfter, 1}} {
		halt := c.waitEvent("a survivor to halt on a dead worker's call", 20*time.Second, func(e haEvent) bool {
			return e.run == k.victim.run && e.phase == "halt" && e.at.After(k.killedAt)
		})
		if halt.effect != k.victim.effect {
			t.Errorf("run %s halted on %q, want the call in flight when its worker died (%s)", k.victim.run, halt.effect, k.victim.effect)
		}
		if done, _ := agent.IsComplete(context.Background(), agenttest.MustJournal(c.s), k.victim.run); done {
			t.Errorf("run %s completed although its in-flight call has an unknown outcome", k.victim.run)
		}
		if n := len(c.effects()[k.victim.run][k.victim.effect]); n != k.fired {
			t.Errorf("run %s: the dead worker's call %s fired %d times, want %d: no one may re-fire it", k.victim.run, k.victim.effect, n, k.fired)
		}
	}

	fx := c.checkAtMostOnce()
	c.checkCompleteRuns(fx, survivors)
	c.checkRecoverSkipsCompleted(survivors)
}

// A worker is SIGSTOPped inside a non-retriable effect (a Step, or a tool call), before the
// effect fires, for well past its lease TTL, while an orchestrator restarts it under the same
// holder name. No other process may take the run while the stalled worker's lease is live. Once
// it lapses, the drivers that take over find the stalled worker's claim and halt with ResumeHalt
// rather than fire the effect. On SIGCONT the stalled worker, still inside the effect, fires the
// effect it owns, loses its lease, and must still journal the outcome, so the run is driven to
// completion afterwards with every effect fired exactly once.
func TestHA_MultiProcessStallPastTTL(t *testing.T) {
	for _, effect := range []string{"reserve", "c1"} {
		t.Run(effect, func(t *testing.T) { stallPastTTL(t, effect) })
	}
}

func stallPastTTL(t *testing.T, effect string) {
	const ttl = 3 * time.Second
	c := newHACluster(t, 4, ttl)
	for _, w := range []string{"w0", "w1"} {
		c.start(w, w)
	}

	victim := c.waitEvent("a worker to enter "+effect, 30*time.Second, func(e haEvent) bool {
		return e.phase == "enter" && e.effect == effect
	})
	c.signal(victim.worker, syscall.SIGSTOP)
	stoppedAt := c.dbNow()
	c.start(victim.worker+"-restarted", victim.worker)

	// The stalled worker renewed its lease at most ttl/2 before the stop, so the lease is live for
	// at least ttl/2 after it; take-over is only possible once it lapses, and must happen within
	// takeoverBound of the stop.
	halt := c.waitEvent("another driver to halt on the stalled worker's claim", c.takeoverBound(), func(e haEvent) bool {
		return e.run == victim.run && e.phase == "halt" && e.worker != victim.worker
	})
	t.Logf("%s halted on the stalled claim %v after the stop (bound %v)", halt.worker, halt.at.Sub(stoppedAt).Round(time.Millisecond), c.takeoverBound())
	if halt.effect != effect {
		t.Errorf("the taking-over driver halted on %q, want the stalled effect (%s)", halt.effect, effect)
	}
	for _, e := range c.events() {
		if e.run == victim.run && e.worker != victim.worker && e.at.After(stoppedAt) && e.at.Before(stoppedAt.Add(ttl/2-300*time.Millisecond)) {
			t.Errorf("worker %s drove run %s (%s %s) %v after its holder stalled, while the holder's lease was still live",
				e.worker, victim.run, e.phase, e.effect, e.at.Sub(stoppedAt))
		}
	}
	if stalled := c.dbNow().Sub(stoppedAt); stalled < 2*ttl {
		time.Sleep(2*ttl - stalled) // keep the worker stopped well past its TTL
	}
	c.signal(victim.worker, syscall.SIGCONT)

	c.waitComplete(c.runs, 40*time.Second)
	fx := c.checkAtMostOnce()
	c.checkCompleteRuns(fx, c.runs)
	if got := fx[victim.run][effect]; len(got) != 1 || got[0] != victim.worker {
		t.Errorf("the stalled effect was fired by %v, want only by the stalled worker %s, which held its claim", got, victim.worker)
	}
	// On waking, the stalled worker's renewer finds its lease gone and ends the drive with
	// ErrLeaseLost.
	c.waitEvent("the stalled worker to end its drive with ErrLeaseLost", 10*time.Second, func(e haEvent) bool {
		return e.run == victim.run && e.worker == victim.worker && e.phase == "lost"
	})
	c.checkRecoverSkipsCompleted(c.runs)
}
