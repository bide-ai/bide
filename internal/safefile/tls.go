package safefile

import "crypto/tls"

// LoadX509KeyPair is tls.LoadX509KeyPair reading the two files as Read does, so that a named pipe
// in place of either is refused instead of waited on (the standard library opens them itself, with
// no way to refuse). Each file is read to MaxDocument bytes.
func LoadX509KeyPair(certFile, keyFile string) (tls.Certificate, error) {
	certPEM, err := Read(certFile, MaxDocument, nil)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := Read(keyFile, MaxDocument, nil)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}
