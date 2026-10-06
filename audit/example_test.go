package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// Wrap a store so every journal write is signed and anchored, run an agent over it, and verify
// the latest anchored tree head: its signature, and that its root is the run's journal root.
func ExampleNewAuditedStore() {
	ctx := context.Background()
	signer := audit.Ed25519Signer{Priv: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))}
	anchor := audit.NewMemAnchorLog() // in production, a log in another trust domain
	store, err := audit.NewAuditedStore(agent.NewMemStore(), signer, anchor)
	if err != nil {
		fmt.Println(err)
		return
	}
	j := agenttest.MustJournal(store)
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("hello")), j)
	if _, err := a.Run(ctx, "run-1", agent.UserText("hi")); err != nil {
		fmt.Println(err)
		return
	}

	// A verifier holds only the public key.
	v, err := audit.VerifierOf(signer)
	if err != nil {
		fmt.Println(err)
		return
	}
	entries := anchor.Entries()
	sth := entries[len(entries)-1].STH
	root, err := audit.Root(ctx, j, "run-1")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("signature valid:", sth.Verify(v) == nil)
	fmt.Println("root matches the journal:", bytes.Equal(sth.Root, root))
	// Output:
	// signature valid: true
	// root matches the journal: true
}
