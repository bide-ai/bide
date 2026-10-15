//go:build ignore

// notify.go writes what the bide audit notifications recording (assets/demo/notifications.sh)
// audits: one SQLite file holding two refund runs that each reached a 2-of-3 approval gate, with
// their notification trails.
//
//   - refund-2001 had a notifier: the real webhook notifier (agent.WithNotifier, notify.Webhook)
//     delivered the waiting approval, signed with HMAC, to a local HTTPS receiver that verified
//     it, and the delivery was acknowledged. The trails are signed (log.pub, heads/*.json).
//   - refund-2002 had no notifier: its approval waits and nobody was told.
//
// Both runs started an hour or more ago on the journal's clock, so the second is past the
// notifier's detection window. Run it from cmd/bide, which carries the modules it imports:
//
//	go run ../../assets/demo/setup/notify.go -dir DIR
//
// DIR gets notify.db, log.pub (the public key that verifies the heads) and heads/: refund-2001.json,
// open-<date>.json and open-index-<year>.json.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/notify"
	"github.com/bide-ai/bide/store/sqlite"
)

func main() {
	dir := flag.String("dir", "", "directory to write notify.db, log.pub and heads/ into")
	flag.Parse()
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "notify.go:", err)
		os.Exit(1)
	}
}

// ticker is a clock that starts at base and moves on a second at each reading, so the notifier's
// claims, backoff and deadlines advance as they would.
type ticker struct {
	mu sync.Mutex
	t  time.Time
}

func (c *ticker) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func run(dir string) error {
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dir, "heads"), 0o755); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "log.pub"), []byte(audit.FormatPublicKey(audit.AlgEd25519, pub)+"\n"), 0o644); err != nil {
		return err
	}
	signer := audit.Ed25519Signer{Priv: priv}

	// The receiver: a local HTTPS endpoint that accepts a delivery only if its signature verifies.
	secret := "demo-webhook-secret"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := notify.Verify(r, []string{secret}, 0); err != nil {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	hook, err := notify.NewWebhook(srv.URL, notify.WithSecrets(secret, ""), notify.WithServerCAPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	if err != nil {
		return err
	}

	s, err := sqlite.Open(filepath.Join(dir, "notify.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	j, err := agent.NewJournal(s)
	if err != nil {
		return err
	}
	es := s.EventStore()

	// Yesterday morning on the journal's clock, so both gates are long past the detection window
	// and every time in the trails is in the past.
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	gate := func(run string, at time.Time, opts ...agent.Option) error {
		refund := agent.MustFunc("refund", "refund an order", func(context.Context, json.RawMessage) (string, error) { return "refunded", nil },
			agent.WithApproval(&policy))
		clock := &ticker{t: at}
		opts = append(opts, agent.WithTools(refund), agent.WithApproverVerifiers(func(string) (agent.ApproverVerifier, bool) { return nil, false }))
		a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c2", "refund", `{"order":"`+run+`","amount":120}`)), j, opts...)
		if err != nil {
			return err
		}
		_, err = a.Run(ctx, run, agent.UserText("Refund "+run), agent.WithClock(clock.now))
		if !errors.As(err, new(*agent.ApprovalPending)) {
			return fmt.Errorf("%s did not pause at its gate: %v", run, err)
		}
		return a.FlushNotifications(ctx)
	}
	nclock := &ticker{t: base}
	if err := gate("refund-2001", base,
		agent.WithNotifier(hook, audit.NewNotifyTrail(es), agent.WithNotifyClock(nclock.now), agent.WithNotifyHolder("notifier-1"))); err != nil {
		return err
	}
	if err := gate("refund-2002", base.Add(time.Hour)); err != nil {
		return err
	}

	// Sign a head over each trail the notifier wrote: the run's, and the open trail's segment and
	// index, which the audit also needs heads for.
	now := time.Now()
	for _, trail := range []string{
		agent.NotifyTrailName("refund-2001"),
		"notify:@open:" + base.Format("2006-01-02"),
		"notify:@open:index:" + base.Format("2006"),
	} {
		leaves, err := es.Load(ctx, trail)
		if err != nil {
			return err
		}
		if len(leaves) == 0 {
			return fmt.Errorf("no trail %s to sign", trail)
		}
		head, err := audit.SignNotifyTrail(ctx, es, trail, signer, now)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(head, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "heads", fileName.Replace(trail)+".json"), append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// fileName makes a trail's name a file name.
var fileName = strings.NewReplacer("notify:@open:", "open-", "notify:", "", ":", "-")
