package emulatortest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/blackwell-systems/aws-kms-emulator/kmstest"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/signer/awskms"
)

// scheme is one signature scheme the signer supports and the KMS key spec that carries it.
type scheme struct {
	name     string
	alg      audit.Alg
	spec     types.KeySpec
	pubLen   int
	sentinel error
	// cfgScheme is what Config.Scheme is set to (empty is the Ed25519 default).
	cfgScheme audit.Alg
}

var schemes = []scheme{
	{"ed25519", audit.AlgEd25519, types.KeySpecEccNistEdwards25519, 32, awskms.ErrNoEd25519, ""},
	{"ecdsa-p256", audit.AlgECDSAP256, types.KeySpecEccNistP256, 65, awskms.ErrNoP256, audit.AlgECDSAP256},
}

func (sc scheme) config(c awskms.API, keyID string) awskms.Config {
	return awskms.Config{Client: c, KeyID: keyID, Scheme: sc.cfgScheme}
}

func newKey(t *testing.T, c *kms.Client, spec types.KeySpec, usage types.KeyUsageType) (id, alias string) {
	t.Helper()
	ctx := context.Background()
	out, err := c.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: spec, KeyUsage: usage})
	if err != nil {
		t.Fatalf("CreateKey %s %s: %v", spec, usage, err)
	}
	id = *out.KeyMetadata.KeyId
	alias = fmt.Sprintf("alias/bide-it-%s", id)
	if _, err := c.CreateAlias(ctx, &kms.CreateAliasInput{AliasName: &alias, TargetKeyId: &id}); err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}
	return id, alias
}

// open creates a key of the scheme's spec and opens a signer on its alias.
func (sc scheme) open(t *testing.T, c *kms.Client) (s *awskms.Signer, id, alias string) {
	t.Helper()
	id, alias = newKey(t, c, sc.spec, types.KeyUsageTypeSignVerify)
	s, err := awskms.Open(context.Background(), sc.config(c, alias))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, id, alias
}

func forEachScheme(t *testing.T, f func(t *testing.T, sc scheme, c *kms.Client)) {
	for _, sc := range schemes {
		t.Run(sc.name, func(t *testing.T) { f(t, sc, kmstest.NewServer(t).Client()) })
	}
}

// Open resolves an alias, passes its self-test, and signs under the key's ARN; signatures verify under
// audit's verifier (for P-256, KMS's DER converted to low-S r||s; 40 signatures cover both S halves).
func TestOpenAndSign(t *testing.T) {
	forEachScheme(t, func(t *testing.T, sc scheme, c *kms.Client) {
		s, id, _ := sc.open(t, c)
		if s.Alg() != sc.alg || len(s.PublicKey()) != sc.pubLen {
			t.Fatalf("Alg %s, public key %d bytes", s.Alg(), len(s.PublicKey()))
		}
		if !strings.HasSuffix(s.KeyARN(), "key/"+id) {
			t.Fatalf("KeyARN %q does not end in key/%s (an alias must resolve to the key's ARN)", s.KeyARN(), id)
		}
		v, err := audit.VerifierOf(s)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 40 {
			msg := bytes.Repeat([]byte{byte(i)}, 1+i*100) // KMS refuses an empty message
			sig, err := s.Sign(msg)
			if err != nil || !v.Verify(msg, sig) {
				t.Fatalf("message %d (%d bytes): %v", i, len(msg), err)
			}
			if v.Verify(append(msg, 'x'), sig) {
				t.Fatalf("message %d: verified over another message", i)
			}
		}
		// The public key in its published form verifies KMS's signature.
		pk, err := audit.ParsePublicKey(audit.FormatPublicKey(sc.alg, s.PublicKey()))
		if err != nil {
			t.Fatal(err)
		}
		if sig := mustSign(t, s, []byte("m")); !pk.Verify([]byte("m"), sig) {
			t.Fatal("the published public key does not verify KMS's signature")
		}
	})
}

func mustSign(t *testing.T, s *awskms.Signer, m []byte) []byte {
	t.Helper()
	sig, err := s.Sign(m)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// The message-size rules: Ed25519 signs a raw message of at most 4096 bytes and refuses more before
// any call; P-256 sends KMS a digest, so a 100 KB message signs.
func TestMessageSize(t *testing.T) {
	forEachScheme(t, func(t *testing.T, sc scheme, c *kms.Client) {
		s, _, _ := sc.open(t, c)
		if _, err := s.Sign(make([]byte, awskms.MaxMessageSize)); err != nil {
			t.Fatalf("a %d-byte message must sign: %v", awskms.MaxMessageSize, err)
		}
		_, err := s.Sign(make([]byte, 100000))
		if sc.alg == audit.AlgEd25519 {
			if !errors.Is(err, awskms.ErrMessageTooLarge) {
				t.Fatalf("err = %v, want ErrMessageTooLarge", err)
			}
		} else if err != nil {
			t.Fatalf("a 100 KB message must sign under P-256: %v", err)
		}
	})
}

// Open signs with the ARN it resolved, so moving the alias to another key afterwards does not change
// which key signs: signatures still verify under the public key read at Open.
func TestAliasMoveCannotChangeTheKey(t *testing.T) {
	forEachScheme(t, func(t *testing.T, sc scheme, c *kms.Client) {
		s, _, alias := sc.open(t, c)
		otherID, _ := newKey(t, c, sc.spec, types.KeyUsageTypeSignVerify)
		if _, err := c.UpdateAlias(context.Background(), &kms.UpdateAliasInput{AliasName: &alias, TargetKeyId: &otherID}); err != nil {
			t.Fatal(err)
		}
		v, _ := audit.VerifierOf(s)
		sig, err := s.Sign([]byte("after the alias moved"))
		if err != nil || !v.Verify([]byte("after the alias moved"), sig) {
			t.Fatalf("sign after alias move: %v", err)
		}
	})
}

// A store anchors a run through a KMS key; the tree heads and an evidence package seal (a fixed 60-byte
// message, so under Ed25519's limit) verify with the public key alone, and not under another KMS key.
func TestAuditedStoreAnchorAndEvidence(t *testing.T) {
	forEachScheme(t, func(t *testing.T, sc scheme, c *kms.Client) {
		ctx := context.Background()
		s, _, _ := sc.open(t, c)
		anchor := audit.NewMemAnchorLog()
		store, err := audit.NewAuditedStore(agent.NewMemStore(), s, anchor)
		if err != nil {
			t.Fatal(err)
		}
		j := agenttest.MustJournal(store)
		refund := agent.MustFunc("refund", "refund a payment", func(context.Context, struct{}) (string, error) { return "refunded", nil })
		var turns []agenttest.ScriptedTurn
		for i := range 12 {
			turns = append(turns, agenttest.ToolTurn(fmt.Sprintf("call-%d", i), "refund", `{}`))
		}
		turns = append(turns, agenttest.TextTurn("done"))
		a := agenttest.MustNew(agenttest.NewScriptedModel(turns...), j, agent.WithTools(refund))
		if _, err := a.Run(ctx, "run-emu", agent.UserText("hi")); err != nil {
			t.Fatal(err)
		}
		pub, err := audit.ParsePublicKey(audit.FormatPublicKey(sc.alg, s.PublicKey()))
		if err != nil {
			t.Fatal(err)
		}
		entries := anchor.Entries()
		if len(entries) == 0 {
			t.Fatal("nothing anchored")
		}
		root, err := audit.Root(ctx, j, "run-emu")
		if err != nil {
			t.Fatal(err)
		}
		sth := entries[len(entries)-1].STH
		if err := sth.Verify(pub); err != nil || !bytes.Equal(sth.Root, root) {
			t.Fatalf("anchored head: verify %v, root match %v", err, bytes.Equal(sth.Root, root))
		}
		for name, opt := range map[string]audit.EvidenceOption{"one action": audit.WithToolCall("call-0"), "all actions": audit.WithAllToolCalls()} {
			ev, err := audit.Evidence(ctx, j, "run-emu", s, 1700000000, opt)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if rep, err := ev.Verify(pub); err != nil || !rep.OK {
				t.Fatalf("%s: %+v %v", name, rep, err)
			}
			other, _, _ := sc.open(t, c)
			otherPub, _ := audit.VerifierOf(other)
			if rep, err := ev.Verify(otherPub); err == nil && rep.OK {
				t.Fatalf("%s: an evidence package verified under another KMS key", name)
			}
		}
	})
}

// Keys of another spec are refused with the scheme's sentinel, from real emulator responses. The
// emulator cannot create a key with a signing-incapable usage on an ECC spec or an ML-DSA key, so the
// usage and signing-algorithm checks in Open are covered by the fake client in signer/awskms.
func TestKeyRefusals(t *testing.T) {
	forEachScheme(t, func(t *testing.T, sc scheme, c *kms.Client) {
		ctx := context.Background()
		wrong := map[string]struct {
			spec  types.KeySpec
			usage types.KeyUsageType
		}{
			"RSA_2048":        {types.KeySpecRsa2048, types.KeyUsageTypeSignVerify},
			"ECC_NIST_P384":   {types.KeySpecEccNistP384, types.KeyUsageTypeSignVerify},
			"ECC_SECG_P256K1": {types.KeySpecEccSecgP256k1, types.KeyUsageTypeSignVerify},
		}
		for _, other := range schemes {
			if other.alg != sc.alg {
				wrong["the other scheme's key ("+other.name+")"] = struct {
					spec  types.KeySpec
					usage types.KeyUsageType
				}{other.spec, types.KeyUsageTypeSignVerify}
			}
		}
		for name, k := range wrong {
			if sc.spec == k.spec && k.usage == types.KeyUsageTypeSignVerify {
				continue
			}
			id, _ := newKey(t, c, k.spec, k.usage)
			if _, err := awskms.Open(ctx, sc.config(c, id)); !errors.Is(err, sc.sentinel) {
				t.Errorf("%s: err = %v, want %v", name, err, sc.sentinel)
			}
		}
		// A symmetric key has no public key: KMS refuses GetPublicKey and no signer is returned.
		id, _ := newKey(t, c, types.KeySpecSymmetricDefault, types.KeyUsageTypeEncryptDecrypt)
		if s, err := awskms.Open(ctx, sc.config(c, id)); err == nil {
			t.Errorf("a symmetric key opened as a signer: %v", s)
		}
		if _, err := awskms.Open(ctx, sc.config(c, "alias/bide-it-no-such-alias")); err == nil {
			t.Error("an unknown alias opened")
		}
	})
}

// A disabled key or one pending deletion is refused at Open, and a key disabled or scheduled for
// deletion after Open makes Sign fail and return no signature.
func TestKeyStates(t *testing.T) {
	forEachScheme(t, func(t *testing.T, sc scheme, c *kms.Client) {
		ctx := context.Background()
		disable := func(id string) error { _, err := c.DisableKey(ctx, &kms.DisableKeyInput{KeyId: &id}); return err }
		schedule := func(id string) error {
			_, err := c.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{KeyId: &id, PendingWindowInDays: ptr(int32(7))})
			return err
		}
		for name, change := range map[string]func(string) error{"disabled": disable, "pending deletion": schedule} {
			id, _ := newKey(t, c, sc.spec, types.KeyUsageTypeSignVerify)
			if err := change(id); err != nil {
				t.Fatal(err)
			}
			if s, err := awskms.Open(ctx, sc.config(c, id)); err == nil {
				t.Errorf("%s key before Open: opened %v", name, s)
			}

			s, id2, _ := sc.open(t, c)
			if err := change(id2); err != nil {
				t.Fatal(err)
			}
			if sig, err := s.Sign([]byte("m")); err == nil || sig != nil {
				t.Errorf("%s key after Open: Sign = %x, %v", name, sig, err)
			}
		}
	})
}

func ptr[T any](v T) *T { return &v }

// stall is an HTTP client whose requests never complete: they return only when the request's context
// is done, so the outcome depends on the deadline or cancellation and not on how fast the emulator is
// or how coarse the clock.
type stall struct{}

func (stall) Do(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// stalling wraps the real client and sends the named operations through stall.
type stalling struct {
	*kms.Client
	getPublicKey, sign bool
}

func (c stalling) GetPublicKey(ctx context.Context, in *kms.GetPublicKeyInput, o ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	if c.getPublicKey {
		o = append(o, func(opts *kms.Options) { opts.HTTPClient = stall{} })
	}
	return c.Client.GetPublicKey(ctx, in, o...)
}

func (c stalling) Sign(ctx context.Context, in *kms.SignInput, o ...func(*kms.Options)) (*kms.SignOutput, error) {
	if c.sign {
		o = append(o, func(opts *kms.Options) { opts.HTTPClient = stall{} })
	}
	return c.Client.Sign(ctx, in, o...)
}

// Open is bounded by Config.Timeout on each KMS call (the public key read and the self-test
// signature) and by the caller's context, whichever KMS call is slow.
func TestOpenHonorsTimeoutAndContext(t *testing.T) {
	forEachScheme(t, func(t *testing.T, sc scheme, c *kms.Client) {
		id, _ := newKey(t, c, sc.spec, types.KeyUsageTypeSignVerify)
		for name, client := range map[string]stalling{
			"GetPublicKey stalls": {Client: c, getPublicKey: true},
			"self-test stalls":    {Client: c, sign: true},
		} {
			cfg := sc.config(client, id)
			cfg.Timeout = 50 * time.Millisecond
			start := time.Now()
			if _, err := awskms.Open(context.Background(), cfg); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s: err = %v, want the deadline", name, err)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("%s: Open took %v with a 50ms timeout", name, d)
			}

			// The caller cancels while the call is stalled; the (long) timeout has not expired.
			cfg.Timeout = time.Hour
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(50*time.Millisecond, cancel)
			if _, err := awskms.Open(ctx, cfg); !errors.Is(err, context.Canceled) {
				t.Errorf("%s, cancelled by the caller: err = %v, want context.Canceled", name, err)
			}
		}

		// A context cancelled before Open makes no signer.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if s, err := awskms.Open(ctx, sc.config(c, id)); !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled context: signer %v, err = %v, want context.Canceled", s, err)
		}
	})
}
