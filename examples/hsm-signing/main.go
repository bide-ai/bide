// Command hsm-signing runs an agent whose audit log is signed by a key held in a
// hardware security module (or SoftHSM2, a software token, for trying it out). The agent refunds
// an order; every journal write is anchored with a tree head signed inside the token; an auditor
// then verifies the anchored head and an evidence package with the public key alone.
//
// Configure it with environment variables (a PIN is never a command-line argument):
//
//	BIDE_PKCS11_MODULE   path of the PKCS#11 library (libsofthsm2.so, a vendor library)
//	BIDE_PKCS11_TOKEN    label of the token
//	BIDE_PKCS11_KEY      label of the Ed25519 signing key (default "bide-audit")
//	BIDE_HSM_PIN         the user PIN, or
//	BIDE_HSM_PIN_FILE    the path of a file that holds it
//
//	go run .
//
// docs/guides/key-custody.md sets up a SoftHSM2 token for it and says what a real HSM needs.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/signer/pkcs11"
)

func main() {
	if err := run(context.Background(), os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hsm-signing:", err)
		os.Exit(1)
	}
}

// config reads the signer configuration from the environment.
func config(getenv func(string) string) (pkcs11.Config, error) {
	cfg := pkcs11.Config{
		Module:     getenv("BIDE_PKCS11_MODULE"),
		TokenLabel: getenv("BIDE_PKCS11_TOKEN"),
		KeyLabel:   getenv("BIDE_PKCS11_KEY"),
		PIN:        pkcs11.PINFromEnv("BIDE_HSM_PIN"),
	}
	if cfg.KeyLabel == "" {
		cfg.KeyLabel = "bide-audit"
	}
	if f := getenv("BIDE_HSM_PIN_FILE"); f != "" {
		cfg.PIN = pkcs11.PINFromFile(f)
	}
	if cfg.Module == "" || cfg.TokenLabel == "" {
		return cfg, errors.New("set BIDE_PKCS11_MODULE and BIDE_PKCS11_TOKEN (see the comment at the top of main.go)")
	}
	return cfg, nil
}

func run(ctx context.Context, getenv func(string) string, out io.Writer) error {
	cfg, err := config(getenv)
	if err != nil {
		return err
	}

	// The signer: the private key stays in the token; bide sends it each message to sign.
	signer, err := pkcs11.Open(cfg)
	if err != nil {
		return err
	}
	defer signer.Close()

	// Every journal write is anchored with a tree head the token signs. In production the anchor
	// is a log in another trust domain; MemAnchorLog stands in for it here.
	anchor := audit.NewMemAnchorLog()
	store, err := audit.NewAuditedStore(agent.NewMemStore(), signer, anchor)
	if err != nil {
		return err
	}
	j := agenttest.MustJournal(store)
	refund := agent.MustFunc("refund", "refund a payment", func(context.Context, struct{}) (string, error) {
		return "refunded", nil
	})
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.ToolTurn("refund-1", "refund", `{}`), agenttest.TextTurn("done")),
		j, agent.WithTools(refund))
	if _, err := a.Run(ctx, "run-1", agent.UserText("refund order 1042")); err != nil {
		return err
	}

	// The auditor holds only the public key: the text form below is what "bide audit verify-*
	// -pubkey" takes.
	pubText := audit.FormatPublicKey(audit.AlgEd25519, signer.PublicKey())
	fmt.Fprintln(out, "log public key:", pubText)
	v, err := audit.ParsePublicKey(pubText)
	if err != nil {
		return err
	}
	entries := anchor.Entries()
	if len(entries) == 0 {
		return errors.New("nothing was anchored")
	}
	sth := entries[len(entries)-1].STH
	root, err := audit.Root(ctx, j, "run-1")
	if err != nil {
		return err
	}
	if err := sth.Verify(v); err != nil || !bytes.Equal(sth.Root, root) {
		return fmt.Errorf("the anchored tree head does not verify: %v", err)
	}
	fmt.Fprintf(out, "anchored tree head: size %d, signature valid, root is the journal's root\n", sth.Size)

	// An evidence package proving the refund ran, sealed by the token.
	ev, err := audit.Evidence(ctx, j, "run-1", signer, sth.TimestampNanos, audit.WithToolCall("refund-1"))
	if err != nil {
		return err
	}
	rep, err := ev.Verify(v)
	if err != nil || !rep.OK {
		return fmt.Errorf("the evidence does not verify: %v", err)
	}
	fmt.Fprintln(out, "evidence for refund-1: sealed by the token, verified with the public key alone")
	return nil
}
