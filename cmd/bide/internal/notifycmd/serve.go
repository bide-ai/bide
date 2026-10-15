package notifycmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bide-ai/bide/notify"
)

// selfSigned is a certificate made for bide notify listen, with the files and hash a sender needs.
type selfSigned struct {
	cert tls.Certificate
	pem  []byte // the certificate, PEM: the "CA" a sender passes to -ca
	pin  string // the key's SPKI SHA-256 as hex, for -pin
	// names are the host names and addresses the certificate is valid for.
	names []string
}

// newSelfSigned makes a certificate for localhost, 127.0.0.1 and ::1 and for extra (the host of the
// listen address, if it is not one of those). It is its own CA: a sender that trusts this one
// certificate (-ca) verifies the receiver by the normal chain check, with the host name checked.
func newSelfSigned(extra []string, now time.Time) (*selfSigned, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "bide notify listen"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	names := []string{"localhost", "127.0.0.1", "::1"}
	for _, h := range extra {
		if h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "0.0.0.0" || h == "::" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
		names = append(names, h)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &selfSigned{
		cert:  tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		pem:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pin:   notify.SPKIHash(leaf),
		names: names,
	}, nil
}

// writeTemp writes b to a new file only the user can read, in the temp directory, and returns its path.
func writeTemp(pattern string, b []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// serverTLS builds the TLS config of a receiver: TLS 1.2 at least, the certificate, and a required,
// verified client certificate if clientCAFile names a CA bundle.
func serverTLS(cert tls.Certificate, clientCAFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	if clientCAFile == "" {
		return cfg, nil
	}
	b, err := readFile(clientCAFile)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return nil, fmt.Errorf("reading -client-ca %s: %v", clientCAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("-client-ca %s holds no certificate", clientCAFile)
	}
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	cfg.ClientCAs = pool
	return cfg, nil
}

// logWriter adapts a printf-style logger to the io.Writer a log.Logger writes to.
type logWriter func(string, ...any)

func (f logWriter) Write(p []byte) (int, error) {
	f("%s", strings.TrimSpace(string(p)))
	return len(p), nil
}

// serve serves h on ln (over TLS if cfg is not nil) until ctx ends, then shuts down, letting a
// request in flight finish for up to ten seconds. It returns nil after a clean stop.
func serve(ctx context.Context, ln net.Listener, h http.Handler, cfg *tls.Config, errLog func(string, ...any)) error {
	srv := &http.Server{
		Handler:           h,
		TLSConfig:         cfg,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second, // above the relay's forwarding budget
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// A failed handshake (an untrusted client, a missing client certificate) is worth a line,
		// and the default logger would write it to the process's log.
		ErrorLog: log.New(logWriter(errLog), "", 0),
	}
	done := make(chan error, 1)
	go func() {
		if cfg != nil {
			done <- srv.ServeTLS(ln, "", "")
		} else {
			done <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		errLog("shutdown: %v", err)
	}
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
