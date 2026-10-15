// Command intake runs a real approval intake with the approve page for the v0.17.0 screenshots: an
// agent paused at a refund tool gated 2 of {finance, ops, risk}, finance already approved. It
// enrols the ops key the browser page creates when the page's public key appears in ENROL_DIR/pub.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/approvals"
	"github.com/bide-ai/bide/audit"
)

type refundModel struct{}

func (refundModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	done := false
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if _, ok := p.(agent.ToolResult); ok {
				done = true
			}
		}
	}
	if done {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "refunded"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	} else {
		args := `{"order_id":"ORD-48213","customer":"Mara Lindqvist","amount_cents":18450,"currency":"USD","reason":"duplicate charge on the 2026-10-12 renewal","method":"original card"}`
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "refund-1", Name: "refund", ArgsFragment: json.RawMessage(args)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func main() {
	enrol := os.Getenv("ENROL_DIR")
	privs := map[string]ed25519.PrivateKey{}
	var mu sync.RWMutex
	enrolled := map[string]ed25519.PublicKey{}
	for _, id := range []string{"finance", "ops", "risk"} {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		privs[id], enrolled[id] = priv, pub
	}
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		mu.RLock()
		defer mu.RUnlock()
		pub, ok := enrolled[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: pub}, true
	}
	_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	as, err := audit.NewAuditedStore(agent.NewMemStore(), audit.Ed25519Signer{Priv: logPriv}, audit.NewMemAnchorLog())
	must(err)
	j := must1(agent.NewJournal(as))
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"finance", "ops", "risk"}}
	run := func() error {
		refund, err := agent.Func("refund", "refund a customer's order", func(context.Context, struct {
			OrderID     string `json:"order_id"`
			Customer    string `json:"customer"`
			AmountCents int    `json:"amount_cents"`
			Currency    string `json:"currency"`
			Reason      string `json:"reason"`
			Method      string `json:"method"`
		}) (string, error) {
			return "ok", nil
		}, agent.WithApproval(&policy))
		if err != nil {
			return err
		}
		a, err := agent.New(refundModel{}, j, agent.WithTools(refund), agent.WithApproverVerifiers(verifiers))
		if err != nil {
			return err
		}
		_, err = a.Run(context.Background(), "refund-1043", agent.UserText("refund order ORD-48213"))
		return err
	}
	if err := run(); !agent.IsPause(err) {
		panic(fmt.Sprint("did not pause: ", err))
	}
	h, err := approvals.New(j, verifiers, approvals.WithPage(), approvals.WithRateLimit(0, 0), approvals.WithGlobalRateLimit(0, 0),
		approvals.WithAuth(func(*http.Request, approvals.Access) error { return nil }),
		approvals.WithGatePolicy(func(tool string) (*agent.ApprovalPolicy, bool) { return &policy, tool == "refund" }),
		approvals.WithOnError(func(err error) { fmt.Fprintln(os.Stderr, "intake:", err) }))
	must(err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	go http.Serve(ln, h)
	base := "http://" + ln.Addr().String()

	// finance has already approved, signed as a person's tool would: through the intake.
	recs, _ := j.History(context.Background(), "refund-1043")
	_, call, _ := agent.FindToolCall(recs, "refund-1")
	subj := agent.ApprovalSubject{RunID: "refund-1043", ToolUseID: "refund-1", ToolName: call.Name, Args: call.Args}
	at := time.Now().UTC().Add(-14 * time.Minute)
	sig := ed25519.Sign(privs["finance"], agent.ApprovalDecisionBytes(subj, "finance", true, at))
	b, _ := json.Marshal(map[string]any{"run": "refund-1043", "call": "refund-1", "approver": "finance", "approved": true,
		"decided_at": agent.FormatTimestamp(at), "alg": string(audit.AlgEd25519), "signature": base64.StdEncoding.EncodeToString(sig)})
	resp, err := http.Post(base+"/decisions", "application/json", bytes.NewReader(b))
	must(err)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		panic(resp.Status)
	}
	fmt.Println("URL " + base + "/page/#run=refund-1043&call=refund-1")
	os.WriteFile(filepath.Join(enrol, "url"), []byte(base+"/page/#run=refund-1043&call=refund-1"), 0o644)
	for {
		time.Sleep(100 * time.Millisecond)
		if b, err := os.ReadFile(filepath.Join(enrol, "pub")); err == nil {
			raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(b)), "ed25519:"))
			if err == nil && len(raw) == 32 {
				mu.Lock()
				enrolled["ops"] = ed25519.PublicKey(raw)
				mu.Unlock()
				os.WriteFile(filepath.Join(enrol, "enrolled"), nil, 0o644)
				fmt.Println("enrolled ops")
				os.Remove(filepath.Join(enrol, "pub"))
			}
		}
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func must1[T any](v T, err error) T { must(err); return v }
