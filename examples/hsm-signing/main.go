// Command hsm-signing runs an agent whose audit log is signed by a key held in a
// hardware security module (or SoftHSM2, a software token, for trying it out) or in Google Cloud
// KMS. The agent refunds an order; every journal write is anchored with a tree head signed inside
// the token or by Cloud KMS; an auditor then verifies the anchored head and an evidence package with
// the public key alone.
//
// Configure a PKCS#11 token with environment variables (a PIN is never a command-line argument):
//
//	BIDE_PKCS11_MODULE   path of the PKCS#11 library (libsofthsm2.so, a vendor library)
//	BIDE_PKCS11_TOKEN    label of the token
//	BIDE_PKCS11_KEY      label of the signing key (default "bide-audit")
//	BIDE_PKCS11_SCHEME   "ed25519" (default) or "ecdsa-p256", the scheme of the key
//	BIDE_HSM_PIN         the user PIN, or
//	BIDE_HSM_PIN_FILE    the path of a file that holds it
//
// Or set BIDE_GCPKMS_KEY_VERSION to use Google Cloud KMS instead of a token:
//
//	BIDE_GCPKMS_KEY_VERSION  the full CryptoKeyVersion name,
//	                         projects/P/locations/L/keyRings/R/cryptoKeys/K/cryptoKeyVersions/N
//	BIDE_GCPKMS_SCHEME       "ed25519" (default) or "ecdsa-p256", the algorithm of the key version
//	BIDE_GCPKMS_ALLOW_SOFTWARE  "1" to accept a SOFTWARE key version (Ed25519 is software only);
//	                         without it only an HSM key version opens, so use ecdsa-p256 with an HSM key
//	BIDE_GCPKMS_ALLOW_EXTERNAL  "1" to accept an EXTERNAL or EXTERNAL_VPC (Cloud EKM) key version
//
// Cloud KMS authenticates with Application Default Credentials (gcloud auth application-default
// login, or a workload identity); no credential is read from the environment by this program.
//
//	go run .
//
// docs/guides/key-custody.md sets up a SoftHSM2 token and a Cloud KMS key for it and says what a
// real HSM needs.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	kms "cloud.google.com/go/kms/apiv1"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/signer/gcpkms"
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
	if sc := getenv("BIDE_PKCS11_SCHEME"); sc != "" {
		cfg.Scheme = audit.Alg(sc)
	}
	if cfg.KeyLabel == "" {
		cfg.KeyLabel = "bide-audit"
	}
	if f := getenv("BIDE_HSM_PIN_FILE"); f != "" {
		cfg.PIN = pkcs11.PINFromFile(f)
	}
	if cfg.Module == "" || cfg.TokenLabel == "" {
		return cfg, errors.New("set BIDE_PKCS11_MODULE and BIDE_PKCS11_TOKEN, or BIDE_GCPKMS_KEY_VERSION (see the comment at the top of main.go)")
	}
	return cfg, nil
}

// newKMSClient creates the Cloud KMS client; a test replaces it to talk to an emulator.
var newKMSClient = func(ctx context.Context) (gcpkms.API, func(), error) {
	c, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c, func() { c.Close() }, nil
}

// gcpConfig reads the Cloud KMS signer configuration from the environment.
func gcpConfig(getenv func(string) string) gcpkms.Config {
	cfg := gcpkms.Config{KeyVersion: getenv("BIDE_GCPKMS_KEY_VERSION"),
		AllowSoftware: getenv("BIDE_GCPKMS_ALLOW_SOFTWARE") == "1", AllowExternal: getenv("BIDE_GCPKMS_ALLOW_EXTERNAL") == "1"}
	if sc := getenv("BIDE_GCPKMS_SCHEME"); sc != "" {
		cfg.Scheme = audit.Alg(sc)
	}
	return cfg
}

// openSigner opens the Cloud KMS signer when BIDE_GCPKMS_KEY_VERSION is set and the PKCS#11 signer
// otherwise. The private key stays in the token or in Cloud KMS; bide sends it each message to sign.
func openSigner(ctx context.Context, getenv func(string) string) (audit.Signer, func(), error) {
	if getenv("BIDE_GCPKMS_KEY_VERSION") != "" {
		client, closeClient, err := newKMSClient(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("cloud kms client: %w", err)
		}
		cfg := gcpConfig(getenv)
		cfg.Client = client
		s, err := gcpkms.Open(ctx, cfg)
		if err != nil {
			closeClient()
			return nil, nil, err
		}
		return s, closeClient, nil
	}
	cfg, err := config(getenv)
	if err != nil {
		return nil, nil, err
	}
	s, err := pkcs11.Open(cfg)
	if err != nil {
		return nil, nil, err
	}
	return s, func() { s.Close() }, nil
}

func run(ctx context.Context, getenv func(string) string, out io.Writer) error {
	signer, closeSigner, err := openSigner(ctx, getenv)
	if err != nil {
		return err
	}
	defer closeSigner()

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
	pubText := audit.FormatPublicKey(signer.Alg(), signer.PublicKey())
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
	fmt.Fprintln(out, "evidence for refund-1: sealed by the key service or token, verified with the public key alone")
	return nil
}
