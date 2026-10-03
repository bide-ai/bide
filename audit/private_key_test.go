package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// badKeys are private keys of the wrong length: ed25519.Sign panics on each.
var badKeys = map[string]ed25519.PrivateKey{"nil": nil, "short": make(ed25519.PrivateKey, 31), "seed only": make(ed25519.PrivateKey, ed25519.SeedSize)}

// catch runs fn and returns what it panicked with, or nil.
func catch(fn func()) (p any) {
	defer func() { p = recover() }()
	fn()
	return nil
}

// AuditedStore promises that anchoring never fails a durable step once the step is recorded. A
// signing key of the wrong length must therefore be refused when the store is built, with an
// ErrConfig error (not a panic), before any step is recorded.
func TestAuditedStore_BadKeyIsRefusedBeforeAnyStep(t *testing.T) {
	for name, priv := range badKeys {
		ctx := context.Background()
		inner := agent.NewMemStore()
		j := agenttest.MustJournal(inner)
		var store *audit.AuditedStore
		var err error
		if p := catch(func() { store, err = audit.NewAuditedStore(inner, edS(priv), audit.NewMemAnchorLog()) }); p != nil {
			t.Errorf("%s key: NewAuditedStore panicked: %v", name, p)
			continue
		}
		if errors.Is(err, agent.ErrConfig) && store == nil {
			continue // refused at construction: nothing was written
		}
		p := catch(func() {
			_, _ = journaltest.Do(ctx, agenttest.MustJournal(store), "r", "s", func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
			})
		})
		recs, _ := j.History(ctx, "r")
		t.Errorf("%s key: NewAuditedStore accepted it (err %v); Do then panicked (%v) with %d records already written", name, err, p, len(recs))
	}
}

// SignAbsenceRoot and CertifyRun return errors, so a key of the wrong length is one of them, not a
// panic.
func TestSigningWithBadKey_IsAnError(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	j := agenttest.MustJournal(store)
	if _, err := journaltest.Do(ctx, j, "r", "s", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, "r")
	th, err := audit.NewTreeHead(ctx, j, "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	_, good, _ := ed25519.GenerateKey(rand.Reader)
	sth := signTH(t, th, good)
	for name, priv := range badKeys {
		calls := map[string]func() error{
			"SignAbsenceRoot": func() error {
				_, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, th, edS(priv), 2)
				return err
			},
			"SignTreeHead(Ed25519Signer)": func() error {
				_, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
				return err
			},
			"CertifyRun": func() error {
				_, err := audit.CertifyRun(ctx, j, "r", sth, audit.RunCertSpec{Signer: edS(priv), TimestampNanos: 2})
				return err
			},
		}
		for fn, call := range calls {
			var err error
			if p := catch(func() { err = call() }); p != nil {
				t.Errorf("%s with %s key panicked: %v", fn, name, p)
			} else if err == nil {
				t.Errorf("%s with %s key: no error", fn, name)
			} else if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "ed25519") {
				t.Errorf("%s with %s key: error %q is not an ErrConfig naming the ed25519 signer", fn, name, err)
			}
		}
	}
}
