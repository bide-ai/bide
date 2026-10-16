// Package policyfile reads a certificate policy file (audit.ParseIdentityPolicy) from disk, for the
// commands that check a certificate-bound decision outside a running gate.
package policyfile

import (
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

func configErr(path string, err error) error {
	return fmt.Errorf("audit: identity policy file %s: %v: %w", path, err, agent.ErrConfig)
}

// Read reads the policy file at path and compiles it; the anchor files it names are read from the
// policy file's directory. Every failure wraps agent.ErrConfig.
func Read(path string) (*audit.CertIdentity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, configErr(path, err)
	}
	dir := filepath.Dir(path)
	return audit.ParseIdentityPolicy(path, b, func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, rel)) })
}

// ReadSources reads the refresher members of the policy file at path: where each issuing CA's CRLs are
// fetched from, and the certificates of the pinned intermediates (audit.ParseCRLSources).
func ReadSources(path string) (audit.CRLSources, []*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, configErr(path, err)
	}
	dir := filepath.Dir(path)
	return audit.ParseCRLSources(path, b, func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, rel)) })
}
