// Package hsmkeys opens the signer a key reference names (internal/keyref): a key on a PKCS#11
// token through signer/pkcs11, or a key in AWS KMS through signer/awskms. The signer is an
// audit.Signer, so bide approve's flow signs with it as it does with a key file.
package hsmkeys

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"time"

	gcpkmsclient "cloud.google.com/go/kms/apiv1"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/bide-hsm/internal/keyref"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/signer/awskms"
	"github.com/bide-ai/bide/signer/gcpkms"
	"github.com/bide-ai/bide/signer/pkcs11"
)

// Options tune how a key is opened.
type Options struct {
	// AllowExtractable lets a PKCS#11 key be used though the token does not mark it
	// non-extractable and sensitive. By default such a key is refused: an approver key that can be
	// read out of the token defeats the purpose of keeping it there. It does not apply to a KMS
	// key, which cannot be exported.
	AllowExtractable bool
	// Alg is the key's scheme: audit.AlgEd25519 or audit.AlgECDSAP256. Empty detects it: Ed25519
	// is tried first, and a key that is not an Ed25519 key is tried as ECDSA P-256. A key of
	// neither is refused.
	Alg audit.Alg
	// KMSClient returns the KMS client for a region. Nil uses the AWS SDK's default credential
	// chain with that region. A test supplies a fake.
	KMSClient func(ctx context.Context, region string) (awskms.API, error)
	// AllowSoftwareKey lets a Google Cloud KMS key version at protection level SOFTWARE be used.
	// By default only an HSM-backed version is, and Cloud KMS has no HSM-backed Ed25519 key, so an
	// Ed25519 key needs this. It applies to a gcpkms: reference only.
	AllowSoftwareKey bool
	// VerifyAttestation makes a Google Cloud KMS key version prove, by its Cloud HSM attestation,
	// that its key was generated in the HSM and cannot be extracted (signer/gcpkms
	// Config.VerifyAttestation). It applies to a gcpkms: reference only.
	VerifyAttestation bool
	// GCPClient returns the Cloud KMS client. Nil uses Google's application default credentials. A
	// test supplies one connected to an emulator.
	GCPClient func(ctx context.Context) (gcpkms.API, error)
	// PIN, when set, replaces the PIN source the reference names. A test uses it to count how often
	// the token is asked to log in.
	PIN pkcs11.PINSource

	memo *pinMemo // the prompted PIN, for the attempts of one Open
}

// Open opens the signer for ref. The result is an io.Closer (the caller closes it), and says where its
// key is held (Where) and, when the non-extractable check was lifted, says so (Notice).
func Open(ctx context.Context, ref keyref.Ref, o Options) (audit.Signer, error) {
	algs := []audit.Alg{audit.AlgEd25519, audit.AlgECDSAP256}
	if ref.Kind == keyref.KindGCPKMS {
		// P-256 is Cloud KMS's HSM-backed scheme and the signer's default; Ed25519 is software only.
		algs = []audit.Alg{audit.AlgECDSAP256, audit.AlgEd25519}
	}
	if o.Alg != "" {
		if o.Alg != audit.AlgEd25519 && o.Alg != audit.AlgECDSAP256 {
			return nil, fmt.Errorf("-alg %q: want ed25519 or ecdsa-p256: an HSM or KMS key here is one of those", o.Alg)
		}
		algs = []audit.Alg{o.Alg}
	}
	if ref.Kind == keyref.KindPKCS11 && ref.PKCS11.PINProtected && o.Alg == "" {
		// Every attempt is a login the person answers on the token's pad: no guessing the scheme.
		return nil, errors.New("pin-source=protected needs -alg: detecting the scheme would log in, and ask for the PIN on the token's pad, more than once")
	}
	if ref.Kind == keyref.KindPKCS11 && ref.PKCS11.PINPrompt {
		o.memo = &pinMemo{}
		defer o.memo.forget()
	}
	var first error
	for i, alg := range algs {
		var (
			s   audit.Signer
			err error
		)
		switch ref.Kind {
		case keyref.KindPKCS11:
			s, err = openPKCS11(ref.PKCS11, alg, o)
		case keyref.KindAWSKMS:
			s, err = openKMS(ctx, ref.KMS, alg, o)
		case keyref.KindGCPKMS:
			s, err = openGCP(ctx, ref.GCP, alg, o)
		default:
			return nil, errors.New("not a key reference")
		}
		if err == nil {
			// Detection found an Ed25519 key. A key of the other scheme under the same selection
			// (labels are not unique on a token) would be what -alg ecdsa-p256 opens: say so
			// rather than pick one, since the approver cannot see which was meant.
			if o.Alg == "" && ref.Kind == keyref.KindPKCS11 && alg == audit.AlgEd25519 {
				if other, perr := openPKCS11(ref.PKCS11, audit.AlgECDSAP256, o); perr == nil || errors.Is(perr, pkcs11.ErrExtractable) {
					if c, ok := other.(io.Closer); ok && perr == nil {
						c.Close()
					}
					s.(io.Closer).Close()
					return nil, fmt.Errorf("the reference selects both an Ed25519 key and an ECDSA P-256 key on the token: add id=<bytes> to the reference to select one, or name the scheme with -alg: %w", pkcs11.ErrAmbiguous)
				}
			}
			d := described{Signer: s, where: ref.String()}
			if k, ok := s.(interface{ KeyARN() string }); ok && ref.Kind == keyref.KindAWSKMS && strings.Contains(ref.KMS.ARN, ":alias/") {
				d.where += " (resolved to " + k.KeyARN() + ")"
			}
			if g, ok := s.(interface{ ProtectionLevel() string }); ok && ref.Kind == keyref.KindGCPKMS {
				d.where += " (protection level " + g.ProtectionLevel() + ")"
				if g.ProtectionLevel() != "HSM" && g.ProtectionLevel() != "HSM_SINGLE_TENANT" {
					d.notice = "-allow-software-key: this key version is at protection level " + g.ProtectionLevel() + ", not in an HSM"
				}
			}
			if ref.Kind == keyref.KindPKCS11 && o.AllowExtractable {
				d.notice = "-allow-extractable: the token was not required to keep this key non-extractable, so it may be copyable out of the token"
			}
			return d, nil
		}
		// Only "this key is not of the scheme tried" moves on to the next scheme: a wrong PIN, a
		// missing key or a refused permission is the answer, and trying again would repeat it.
		if i < len(algs)-1 && notOfScheme(err, alg) {
			first = err
			continue
		}
		if first != nil && notOfScheme(err, alg) {
			return nil, fmt.Errorf("the key is neither an Ed25519 nor an ECDSA P-256 key (first attempt: %v; second: %w)", first, err)
		}
		return nil, err
	}
	return nil, first
}

// notOfScheme reports whether err says the key is not a key of scheme alg (as opposed to any other
// failure, which is the answer and is not retried under the other scheme).
func notOfScheme(err error, alg audit.Alg) bool {
	if alg == audit.AlgEd25519 {
		return errors.Is(err, pkcs11.ErrNoEd25519) || errors.Is(err, awskms.ErrNoEd25519) || errors.Is(err, gcpkms.ErrNoEd25519)
	}
	return errors.Is(err, pkcs11.ErrNoP256) || errors.Is(err, awskms.ErrNoP256) || errors.Is(err, gcpkms.ErrNoP256)
}

// described is a signer with where its key is held and a notice for the approver. It forwards Close.
type described struct {
	audit.Signer
	where, notice string
}

// Where says where the key is held: the reference, without the source of the PIN.
func (d described) Where() string { return d.where }

// Notice is shown beside the key by bide-hsm approve.
func (d described) Notice() string { return d.notice }

// Close closes the underlying signer when it has a session to close.
func (d described) Close() error {
	if c, ok := d.Signer.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// cgoEnabled reports whether this build can load a PKCS#11 module. A test replaces it.
var cgoEnabled = func() bool {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "CGO_ENABLED" {
				return s.Value != "0"
			}
		}
	}
	return true
}

func openPKCS11(p *keyref.PKCS11, alg audit.Alg, o Options) (audit.Signer, error) {
	// Before any PIN is read: a build that cannot load a PKCS#11 module says so, not that the
	// PIN is missing.
	if !cgoEnabled() {
		return nil, pkcs11.ErrNoCgo
	}
	cfg := pkcs11.Config{
		Module:                p.ModulePath,
		TokenLabel:            p.TokenLabel,
		SlotID:                p.SlotID,
		KeyLabel:              p.ObjectLabel,
		KeyID:                 p.ID,
		Scheme:                alg,
		RequireNonExtractable: !o.AllowExtractable,
	}
	switch {
	case p.PINFile != "":
		cfg.PIN = pinFromFile(p.PINFile)
	case p.PINProtected:
		cfg.ProtectedAuthPath = true // the token collects the PIN itself
	case p.PINPrompt:
		label := p.TokenLabel
		if label == "" && p.SlotID != nil {
			label = fmt.Sprintf("slot %d", *p.SlotID)
		}
		if o.memo != nil {
			cfg.PIN = o.memo.source(label)
		} else {
			cfg.PIN = promptPIN(label)
		}
	case p.PINEnv != "":
		cfg.PIN = pkcs11.PINFromEnv(p.PINEnv)
	default:
		cfg.PIN = pkcs11.PINFromEnv(keyref.DefaultPINEnv)
	}
	if o.PIN != nil {
		cfg.PIN = o.PIN
	}
	s, err := pkcs11.Open(cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// maxPINFile bounds what is read from a PIN file; a PIN is a few dozen characters.
const maxPINFile = 1024

// pinFromFile reads the PIN from the file at path each time a session opens (so a rotated PIN is
// picked up). It refuses a file that is not a regular file or, on Unix, that group or others can
// read or write, as bide approve refuses a key file: the mode is read from the open file, so what is
// checked is what is read. It drops one trailing line break. An error names the path, never
// content.
func pinFromFile(path string) pkcs11.PINSource {
	return func() (string, error) {
		f, err := approvecmd.OpenPrivate(path, "PIN file")
		if err != nil {
			if _, ok := errors.AsType[*approvecmd.PrivateFileError](err); ok {
				return "", err
			}
			return "", fmt.Errorf("PIN file %s: cannot open it", path)
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxPINFile+1))
		if err != nil {
			return "", fmt.Errorf("PIN file %s: cannot read it", path)
		}
		if len(b) > maxPINFile {
			return "", fmt.Errorf("PIN file %s is too long to be a PIN", path)
		}
		pin := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		if pin == "" {
			return "", fmt.Errorf("PIN file %s is empty", path)
		}
		return pin, nil
	}
}

func openKMS(ctx context.Context, k *keyref.KMS, alg audit.Alg, o Options) (audit.Signer, error) {
	mk := o.KMSClient
	if mk == nil {
		mk = defaultKMS
	}
	c, err := mk(ctx, k.Region)
	if err != nil {
		return nil, credError("awskms: could not load AWS credentials or configuration; set AWS_PROFILE or the standard AWS environment, or check the shared config files", err)
	}
	s, err := awskms.Open(ctx, awskms.Config{Client: c, KeyID: k.ARN, Scheme: alg})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func openGCP(ctx context.Context, g *keyref.GCPKMS, alg audit.Alg, o Options) (audit.Signer, error) {
	mk := o.GCPClient
	if mk == nil {
		mk = defaultGCP
	}
	c, err := mk(ctx)
	if err != nil {
		return nil, credError("gcpkms: could not load Google application default credentials; run gcloud auth application-default login or set GOOGLE_APPLICATION_CREDENTIALS", err)
	}
	s, err := gcpkms.Open(ctx, gcpkms.Config{Client: c, KeyVersion: g.Version, Scheme: alg,
		AllowSoftwareKey: o.AllowSoftwareKey, VerifyAttestation: o.VerifyAttestation})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func defaultGCP(ctx context.Context) (gcpkms.API, error) {
	ctx, cancel := context.WithTimeout(ctx, configTimeout)
	defer cancel()
	return gcpkmsclient.NewKeyManagementClient(ctx)
}

// credError is a fixed message for a failure to load cloud credentials or configuration. The SDKs'
// errors can echo the content of a malformed credentials or config file (a character, a type value, a
// parse detail), so the message is never built from them; only a context error keeps its identity for
// errors.Is.
func credError(msg string, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: %w", msg, context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", msg, context.Canceled)
	}
	return errors.New(msg)
}

// configTimeout bounds loading the AWS configuration, which can reach for the instance metadata
// service; each KMS call has its own timeout (awskms.DefaultTimeout).
const configTimeout = 30 * time.Second

func defaultKMS(ctx context.Context, region string) (awskms.API, error) {
	ctx, cancel := context.WithTimeout(ctx, configTimeout)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return kms.NewFromConfig(cfg), nil
}

var _ io.Closer = (*pkcs11.Signer)(nil)
