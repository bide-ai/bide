package audit

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// fuzzPriv is a fixed signing key so fuzz seeds signed with it stay valid across runs.
var fuzzPriv = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
var fuzzPub = fuzzPriv.Public().(ed25519.PublicKey)

// fuzzRun journals a small fixed run (a model turn with text and a tool call, its tool result, and
// a final answer) and returns the store, its records, and a signed journal head over it.
func fuzzRun(tb testing.TB) (*agent.Journal, []agent.Record, SignedTreeHead) {
	tb.Helper()
	ctx := context.Background()
	store := agenttest.MemJournal()
	recs := []agent.Record{
		{Name: "@llm/0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{
			agent.Text{Text: "refund $10"},
			agent.ToolUse{ID: "c1", Name: "refund", Args: json.RawMessage(`{"amount":10}`)},
		}}, Usage: &agent.Usage{InputTokens: 3, OutputTokens: 4}},
		{Name: "c1", Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)},
		{Name: "@llm/1", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Text{Text: "done"}}}},
	}
	for _, r := range recs {
		if _, err := journaltest.Do(ctx, store, "run", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			tb.Fatal(err)
		}
	}
	th, err := NewTreeHead(ctx, store, "run", 1000)
	if err != nil {
		tb.Fatal(err)
	}
	got, _ := store.History(ctx, "run")
	return store, got, signTH(tb, th, fuzzPriv)
}

// edS and edV wrap raw ed25519 keys as a Signer and a Verifier.
func edS(priv ed25519.PrivateKey) Signer { return Ed25519Signer{Priv: priv} }
func edV(pub ed25519.PublicKey) Verifier { return Ed25519Verifier{Pub: pub} }

// signTH signs th with priv under ed25519, failing the test on error.
func signTH(tb testing.TB, th TreeHead, priv ed25519.PrivateKey) SignedTreeHead {
	tb.Helper()
	sth, err := SignTreeHead(th, edS(priv))
	if err != nil {
		tb.Fatal(err)
	}
	return sth
}

// forceSignTH signs th under ed25519 without SignTreeHead's shape check, so a test can build a
// head no producer would sign and confirm a verifier refuses it.
func forceSignTH(th TreeHead, priv ed25519.PrivateKey) SignedTreeHead {
	return SignedTreeHead{Format: STHFormat, TreeHead: th, Alg: AlgEd25519, Signature: ed25519.Sign(priv, th.canonical(AlgEd25519))}
}

// recOf decodes the record a bundle proves, failing the test if it does not decode.
func recOf(tb testing.TB, b ProofBundle) agent.Record {
	tb.Helper()
	r, err := b.Record()
	if err != nil {
		tb.Fatal(err)
	}
	return r
}

// editRec decodes the record b proves, applies edit to it, and puts its journal encoding back
// in b: a tampered bundle.
func editRec(tb testing.TB, b *ProofBundle, edit func(*agent.Record)) {
	tb.Helper()
	r := recOf(tb, *b)
	edit(&r)
	enc, err := agent.EncodeRecord(r)
	if err != nil {
		tb.Fatal(err)
	}
	b.RecordBytes = enc
}

// reportErr checks a report verifier's error against its verdict: nil when ok, ErrNotVerified when
// not. It returns a description of any other combination, or nil.
func reportErr(ok bool, err error) error {
	switch {
	case ok && err == nil, !ok && errors.Is(err, ErrNotVerified):
		return nil
	case ok:
		return fmt.Errorf("verdict OK with error %w", err)
	default:
		return fmt.Errorf("verdict not OK with error %v, want ErrNotVerified", err)
	}
}
