package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
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
// signing key of the wrong length must therefore be refused when the store is built, not panic in
// Do after the inner store has already recorded the step.
func TestAuditedStore_BadKeyIsRefusedBeforeAnyStep(t *testing.T) {
	for name, priv := range badKeys {
		ctx := context.Background()
		inner := agent.NewMemStore()
		var store *audit.AuditedStore
		if catch(func() { store = audit.NewAuditedStore(inner, priv, audit.NewMemAnchorLog()) }) != nil {
			continue // refused at construction: nothing was written
		}
		p := catch(func() {
			_, _ = store.Do(ctx, "r", "s", func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
			})
		})
		recs, _ := inner.History(ctx, "r")
		t.Errorf("%s key: NewAuditedStore accepted it; Do then panicked (%v) with %d records already written", name, p, len(recs))
	}
}

// SignAbsenceRoot and CertifyRun return errors, so a key of the wrong length is one of them, not a
// panic.
func TestSigningWithBadKey_IsAnError(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "r", "s", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := store.History(ctx, "r")
	th, err := audit.NewTreeHead(ctx, store, "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	_, good, _ := ed25519.GenerateKey(rand.Reader)
	sth := audit.SignTreeHead(th, good)
	for name, priv := range badKeys {
		calls := map[string]func() error{
			"SignAbsenceRoot": func() error {
				_, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, th, priv, 2)
				return err
			},
			"CertifyRun": func() error {
				_, err := audit.CertifyRun(ctx, store, "r", sth, audit.RunCertSpec{}, priv, 2)
				return err
			},
		}
		for fn, call := range calls {
			var err error
			if p := catch(func() { err = call() }); p != nil {
				t.Errorf("%s with %s key panicked: %v", fn, name, p)
			} else if err == nil {
				t.Errorf("%s with %s key: no error", fn, name)
			} else if want := fmt.Sprintf("%d bytes", len(priv)); !strings.Contains(err.Error(), want) {
				t.Errorf("%s with %s key: error %q does not give the key's length (%s)", fn, name, err, want)
			}
		}
	}
}
