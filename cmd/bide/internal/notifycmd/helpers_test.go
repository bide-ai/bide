package notifycmd

import (
	"crypto/tls"
	"crypto/x509"
)

func tlsWith(pool *x509.CertPool) *tls.Config {
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}
