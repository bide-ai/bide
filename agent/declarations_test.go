package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/declarations"
)

var updateDeclarations = flag.Bool("update-declarations", false, "rewrite internal/declarations/testdata/agent.golden.json")

func declAgent(t *testing.T, policy *TaintPolicy, tools ...Tool) *Agent {
	t.Helper()
	opts := []Option{WithTools(tools...), noKeys()}
	if policy != nil {
		opts = append(opts, WithTaintPolicy(*policy))
	}
	return mustNew(&scriptModel{turns: [][]Emit{textTurn("x")}}, memJournal(), opts...)
}

// The document an agent dumps carries the digest its runs record, and the document's own recomputation of
// that digest (internal/declarations) is the same bytes: one definition of what the digest covers.
func TestDeclarations_TheDigestIsTheOneRunsRecord(t *testing.T) {
	read := MustFunc("read", "read", func(ctx context.Context, _ struct{}) (string, error) { return "", nil }, WithSafety(Safety{ReadOnly: true}), Untrusted("crm"))
	pay := refundTool(`refund_one: refund.amount[minor, currency=currency] > 500 USD -> refuse`)
	exempt := MustFunc("notify", "notify", func(context.Context, struct{}) (string, error) { return "", nil }, TaintExempt("reviewed by risk"))
	gated := MustFunc("wire", "wire", func(context.Context, struct{}) (string, error) { return "", nil }, WithApproval(&ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}, Timeout: time.Minute}))
	for _, tc := range []struct {
		name   string
		policy *TaintPolicy
	}{
		{"no policy", nil},
		{"refuse", &TaintPolicy{Refuse: true}},
		{"single gate", &TaintPolicy{Gate: SingleApproval()}},
		{"single gate with a timeout", &TaintPolicy{Gate: &ApprovalPolicy{Need: 1, Timeout: time.Second, one: true}}},
		{"m-of-n", &TaintPolicy{Gate: &ApprovalPolicy{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true, Timeout: 90 * time.Second}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := declAgent(t, tc.policy, read, pay, exempt, gated)
			d := a.declarationsDoc()
			if d.Digest != a.taintDigest() {
				t.Fatalf("document digest %q, agent digest %q", d.Digest, a.taintDigest())
			}
			if got := d.Recompute(); got != d.Digest {
				t.Fatalf("the report recomputes digest %q from the document, the agent computes %q: they disagree about what the digest covers", got, d.Digest)
			}
			b, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			back, err := declarations.Decode(bytes.NewReader(b))
			if err != nil || back.Digest != d.Digest {
				t.Fatalf("the document does not read back: %v", err)
			}
		})
	}
}

// The document lists every registered tool, sorted, with what the report needs to say about it.
func TestDeclarations_ListsEveryTool(t *testing.T) {
	read := MustFunc("read", "read", func(context.Context, struct{}) (string, error) { return "", nil }, WithSafety(Safety{ReadOnly: true}), Untrusted("crm"))
	write := MustFunc("write", "write", func(context.Context, struct{}) (string, error) { return "", nil }, WithApproval(SingleApproval()))
	d := declAgent(t, &TaintPolicy{Gate: SingleApproval()}, write, read).declarationsDoc()
	if len(d.Tools) != 2 || d.Tools[0].Name != "read" || d.Tools[1].Name != "write" {
		t.Fatalf("tools = %+v, want read then write", d.Tools)
	}
	if d.Tools[0].Effect || d.Tools[0].Untrusted != "crm" || !d.Tools[1].Effect || d.Tools[1].OwnGate == nil || !d.Tools[1].OwnGate.Single {
		t.Fatalf("tools = %+v", d.Tools)
	}
}

// The golden document is what the CLI's own tests read; it is rewritten with -update-declarations.
func TestDeclarations_Golden(t *testing.T) {
	read := MustFunc("read_email", "read", func(context.Context, struct{}) (string, error) { return "", nil }, WithSafety(Safety{ReadOnly: true}), Untrusted("inbound-email"))
	pay := refundTool(`refund_one: refund.amount[minor, currency=currency] > 500 USD -> refuse`)
	d := declAgent(t, &TaintPolicy{Gate: &ApprovalPolicy{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true}}, read, pay).declarationsDoc()
	var buf bytes.Buffer
	if err := declarations.Encode(&buf, d); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	path := filepath.Join("..", "internal", "declarations", "testdata", "agent.golden.json")
	if *updateDeclarations {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./agent -run TestDeclarations_Golden -update-declarations)", err)
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("the declarations document changed; rewrite the golden with -update-declarations\n got: %s\nwant: %s", b, want)
	}
}

// Declarations writes the document, and it is the one the golden holds.
func TestDeclarations_TheExportedMethodWritesTheDocument(t *testing.T) {
	read := MustFunc("read_email", "read", func(context.Context, struct{}) (string, error) { return "", nil }, WithSafety(Safety{ReadOnly: true}), Untrusted("inbound-email"))
	pay := refundTool(`refund_one: refund.amount[minor, currency=currency] > 500 USD -> refuse`)
	a := declAgent(t, &TaintPolicy{Gate: &ApprovalPolicy{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true}}, read, pay)
	got, err := a.Declarations()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("..", "internal", "declarations", "testdata", "agent.golden.json"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Declarations differs from the golden (%v)\n got: %s\nwant: %s", err, got, want)
	}
	d, err := declarations.Decode(bytes.NewReader(got))
	if err != nil || d.Digest != a.taintDigest() {
		t.Fatalf("the document does not read back with the agent's digest: %v", err)
	}
}
