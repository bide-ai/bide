package notify

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// reloadEvery is the most often the client certificate's files are stat'ed.
const reloadEvery = time.Minute

// SPKIHash returns the hex SHA-256 of a certificate's SubjectPublicKeyInfo: the value WithServerPin
// takes, and the value recorded as the client certificate in a delivery's audit record.
func SPKIHash(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// parsePin accepts a pin as 64 hex characters or as base64 (the form HPKP and openssl print).
func parsePin(s string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	if b, err := hex.DecodeString(s); err == nil && len(b) == sha256.Size {
		copy(out[:], b)
		return out, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == sha256.Size {
			copy(out[:], b)
			return out, nil
		}
	}
	return out, errors.New("a pin is the SHA-256 of a SubjectPublicKeyInfo, as 64 hex characters or base64")
}

// pinVerifier returns a tls.Config.VerifyConnection that, after normal chain verification (which
// Go has already done when this runs), requires some certificate of a verified chain to match a
// pin. It never replaces verification: with InsecureSkipVerify off there are always verified chains.
func pinVerifier(pins [][sha256.Size]byte) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.VerifiedChains) == 0 {
			return errors.New("notify: no verified certificate chain to pin against")
		}
		for _, chain := range cs.VerifiedChains {
			for _, c := range chain {
				sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
				for _, p := range pins {
					if subtle.ConstantTimeCompare(sum[:], p[:]) == 1 {
						return nil
					}
				}
			}
		}
		return errors.New("notify: the receiver's key matches no configured pin")
	}
}

// clientCert holds the client certificate and key and re-reads them when their files change.
type clientCert struct {
	certFile, keyFile string // empty when the pair came from PEM
	now               func() time.Time
	onError           func(error)

	mu        sync.Mutex
	cur       *tls.Certificate
	spki      string
	lastCheck time.Time
	stamp     fileStamp
}

// fileStamp identifies the files' state: modification times and sizes for a cheap look, and a hash
// of the contents so a same-size rewrite that keeps its modification time is still seen.
type fileStamp struct {
	certMod, keyMod   time.Time
	certSize, keySize int64
	sum               [sha256.Size]byte
}

func newClientCertFiles(certFile, keyFile string, now func() time.Time, onError func(error)) (*clientCert, error) {
	c := &clientCert{certFile: certFile, keyFile: keyFile, now: now, onError: onError}
	if err := c.load(); err != nil {
		return nil, err
	}
	c.lastCheck = now()
	return c, nil
}

func newClientCertPEM(certPEM, keyPEM []byte, now func() time.Time, onError func(error)) (*clientCert, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("notify: client certificate: %w", err)
	}
	c := &clientCert{now: now, onError: onError}
	c.set(&cert)
	return c, nil
}

func (c *clientCert) set(cert *tls.Certificate) {
	if cert.Leaf == nil {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			cert.Leaf = leaf
		}
	}
	c.cur = cert
	if cert.Leaf != nil {
		c.spki = SPKIHash(cert.Leaf)
	}
}

func (c *clientCert) stampNow() (fileStamp, error) {
	var s fileStamp
	h := sha256.New()
	for i, f := range []string{c.certFile, c.keyFile} {
		info, err := os.Stat(f)
		if err != nil {
			return s, err
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return s, err
		}
		h.Write(b)
		h.Write([]byte{0})
		if i == 0 {
			s.certMod, s.certSize = info.ModTime(), info.Size()
		} else {
			s.keyMod, s.keySize = info.ModTime(), info.Size()
		}
	}
	copy(s.sum[:], h.Sum(nil))
	return s, nil
}

// load reads the pair from its files; on failure it leaves the current pair in place. Callers hold
// c.mu, or own c exclusively.
func (c *clientCert) load() error {
	stamp, err := c.stampNow()
	if err != nil {
		return fmt.Errorf("notify: client certificate: %w", unwrapPath(err))
	}
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return fmt.Errorf("notify: client certificate: %w", unwrapPath(err))
	}
	c.set(&cert)
	c.stamp = stamp
	return nil
}

func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// get is tls.Config.GetClientCertificate. It returns the current pair, first reloading it if the
// files changed, checking at most once a minute.
func (c *clientCert) get(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.certFile != "" && c.now().Sub(c.lastCheck) >= reloadEvery {
		c.refresh(false)
	}
	if rec, ok := cri.Context().Value(presentedKey{}).(*presented); ok {
		rec.set(c.spki)
	}
	return c.cur, nil
}

// refresh re-reads the files if their stamp changed, or unconditionally when force is set (after a
// handshake failure, which a rotated certificate may explain). A failed read keeps the last good
// pair and reports the error. Callers hold c.mu.
func (c *clientCert) refresh(force bool) {
	c.lastCheck = c.now()
	if c.certFile == "" {
		return
	}
	if !force {
		s, err := c.stampNow()
		if err == nil && s == c.stamp {
			return
		}
	}
	if err := c.load(); err != nil && c.onError != nil {
		c.onError(fmt.Errorf("%w (keeping the last good client certificate)", err))
	}
}

// reloadNow is called after a TLS handshake failure.
func (c *clientCert) reloadNow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh(true)
}

// presented records, per request, that a client certificate was sent in the handshake and which.
type presentedKey struct{}

type presented struct {
	mu   sync.Mutex
	ok   bool
	spki string
}

func (p *presented) set(spki string) {
	p.mu.Lock()
	p.ok, p.spki = true, spki
	p.mu.Unlock()
}

func (p *presented) get() (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ok, p.spki
}

func withPresented(ctx context.Context) (context.Context, *presented) {
	p := &presented{}
	return context.WithValue(ctx, presentedKey{}, p), p
}
