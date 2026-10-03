package agent_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

type checkResult struct {
	Name  string `json:"name"`
	Pass  bool   `json:"pass"`
	Score int    `json:"score"`
}

// TestParallel_DurableAuditableFanIn models a compliance pipeline's parallel-checks stage: run
// three independent checks at once via Parallel, then confirm (a) all results come back in order,
// (b) a resumed run memoizes completed checks instead of re-running them (durable at-most-once),
// and (c) each check is independently provable against a signed tree head (auditable fan-in).
func TestParallel_DurableAuditableFanIn(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	const runID = "kyc-123"

	var sanctions, credit, fraud int64
	checks := []agent.Task[checkResult]{
		{Name: "sanctions_check", Safety: agent.Safety{ReadOnly: true}, Fn: func(context.Context) (checkResult, error) {
			atomic.AddInt64(&sanctions, 1)
			return checkResult{"sanctions", true, 10}, nil
		}},
		{Name: "credit_check", Safety: agent.Safety{ReadOnly: true}, Fn: func(context.Context) (checkResult, error) {
			atomic.AddInt64(&credit, 1)
			return checkResult{"credit", true, 7}, nil
		}},
		{Name: "fraud_check", Safety: agent.Safety{ReadOnly: true}, Fn: func(context.Context) (checkResult, error) {
			atomic.AddInt64(&fraud, 1)
			return checkResult{"fraud", false, 3}, nil
		}},
	}

	results, err := agent.Parallel(ctx, store, runID, checks)
	if err != nil {
		t.Fatalf("Parallel: %v", err)
	}
	// (a) results in task order, all three ran exactly once.
	if len(results) != 3 || results[0].Name != "sanctions" || results[1].Name != "credit" || results[2].Name != "fraud" {
		t.Fatalf("unexpected results order/content: %+v", results)
	}
	if sanctions != 1 || credit != 1 || fraud != 1 {
		t.Fatalf("expected each check to run once, got s=%d c=%d f=%d", sanctions, credit, fraud)
	}

	// (b) resume: same runID + names, completed checks are memoized, not re-run.
	results2, err := agent.Parallel(ctx, store, runID, checks)
	if err != nil {
		t.Fatalf("Parallel (resume): %v", err)
	}
	if sanctions != 1 || credit != 1 || fraud != 1 {
		t.Fatalf("resume re-ran a completed check: s=%d c=%d f=%d", sanctions, credit, fraud)
	}
	if results2[0] != results[0] || results2[2] != results[2] {
		t.Fatalf("resume returned different results: %+v vs %+v", results2, results)
	}

	// (c) each stage is independently provable against a signed tree head.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sanctions_check", "credit_check", "fraud_check"} {
		bundle, err := audit.ProveStep(ctx, store, runID, name, sth)
		if err != nil {
			t.Fatalf("ProveStep %s: %v", name, err)
		}
		if err := bundle.Verify(audit.Ed25519Verifier{Pub: pub}); err != nil {
			t.Fatalf("stage %s proof did not verify: %v", name, err)
		}
	}
}

// TestParallel_PartialFailure confirms the fan-in preserves every result and aggregates errors: a
// failing check surfaces in the joined error, the others still return, and (since a failed step is
// not journaled and a check is ReadOnly) it re-runs on resume while the succeeded ones are memoized.
func TestParallel_PartialFailure(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	const runID = "kyc-456"

	boom := errors.New("credit bureau timeout")
	var creditAttempts int64
	tasks := []agent.Task[checkResult]{
		{Name: "sanctions_check", Safety: agent.Safety{ReadOnly: true}, Fn: func(context.Context) (checkResult, error) {
			return checkResult{"sanctions", true, 10}, nil
		}},
		{Name: "credit_check", Safety: agent.Safety{ReadOnly: true}, Fn: func(context.Context) (checkResult, error) {
			atomic.AddInt64(&creditAttempts, 1)
			return checkResult{}, boom
		}},
	}

	results, err := agent.Parallel(ctx, store, runID, tasks)
	if !errors.Is(err, boom) {
		t.Fatalf("expected the joined error to include the failing check, got %v", err)
	}
	if results[0].Name != "sanctions" {
		t.Fatalf("succeeded check result should still be present: %+v", results)
	}

	// The failed check was not journaled, so a resume re-runs it (correct: nothing completed).
	_, _ = agent.Parallel(ctx, store, runID, tasks)
	if creditAttempts != 2 {
		t.Fatalf("expected the failed check to re-run on resume, attempts=%d", creditAttempts)
	}
	// The succeeded stage is provable; the failed one is not (it produced no record).
	if _, err := audit.ProveStep(ctx, store, runID, "credit_check", mustSTH(t, mustTH(t, ctx, store, runID), mustKey(t))); err == nil {
		t.Fatalf("a failed (unrecorded) step should not be provable")
	}
}

func mustTH(t *testing.T, ctx context.Context, store *agent.Journal, runID string) audit.TreeHead {
	t.Helper()
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	return th
}

func mustSTH(t *testing.T, th audit.TreeHead, priv ed25519.PrivateKey) audit.SignedTreeHead {
	t.Helper()
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatalf("SignTreeHead: %v", err)
	}
	return sth
}

func mustKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return priv
}

// A task that is a side effect runs at most once. When it fails, it may still have taken effect
// (a gateway that timed out after charging), so resume halts on it instead of running it again.
func TestParallel_FailedSideEffectHaltsOnResume(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MemJournal()
	var charges int64
	tasks := []agent.Task[string]{{Name: "charge", Fn: func(context.Context) (string, error) {
		atomic.AddInt64(&charges, 1)
		return "", errors.New("gateway timeout")
	}}}
	if _, err := agent.Parallel(ctx, store, "r1", tasks); err == nil {
		t.Fatal("setup: want the charge's error")
	}
	_, err := agent.Parallel(ctx, store, "r1", tasks)
	var halt *agent.ResumeHalt
	if !errors.As(err, &halt) || halt.Op.ID != "charge" || charges != 1 {
		t.Fatalf("resume: err = %v after %d charges; want *ResumeHalt for charge after 1", err, charges)
	}
}

// Two tasks with one name would share one journal entry, so one would never run.
func TestParallel_RejectsDuplicateNames(t *testing.T) {
	var ran int64
	fn := func(context.Context) (int, error) { atomic.AddInt64(&ran, 1); return 1, nil }
	_, err := agent.Parallel(context.Background(), agenttest.MemJournal(), "r1",
		[]agent.Task[int]{{Name: "check", Fn: fn}, {Name: "check", Fn: fn}})
	if !errors.Is(err, agent.ErrConfig) || ran != 0 {
		t.Fatalf("err = %v, ran = %d; want ErrConfig before any task runs", err, ran)
	}
}
