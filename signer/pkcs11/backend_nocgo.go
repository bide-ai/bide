//go:build !cgo

package pkcs11

// openSession cannot load a PKCS#11 module without cgo.
func openSession(Config, string) (session, []byte, error) { return nil, nil, ErrNoCgo }
