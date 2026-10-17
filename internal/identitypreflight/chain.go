package identitypreflight

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// maxChainFile bounds what is read from a certificate file: the longest chain a decision may carry in PEM,
// and slack (the bound bide approve -cert applies).
const maxChainFile = agent.MaxDecisionCertChain * agent.MaxDecisionCertDER * 2

// readChain reads certificates from a PEM file: CERTIFICATE blocks only (a private key block is refused by
// name and never printed), nothing outside the blocks, bounded in count and size like a decision's chain.
// flag names the option for messages. A file that cannot be read is an IOError; one that is not a chain is
// a finding, returned as the error text.
func readChain(path, flag string) (der [][]byte, problem string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", &IOError{Err: fmt.Errorf("%s: %w", flag, err)}
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxChainFile+1))
	if err != nil {
		return nil, "", &IOError{Err: fmt.Errorf("%s: %w", flag, err)}
	}
	if len(b) > maxChainFile {
		return nil, fmt.Sprintf("%s is larger than %d bytes: it is not a certificate chain", flag, maxChainFile), nil
	}
	rest := b
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			if strings.Contains(blk.Type, "PRIVATE KEY") {
				return nil, fmt.Sprintf("%s holds a private key block: give the certificate chain only, and keep the private key out of it", flag), nil
			}
			return nil, fmt.Sprintf("%s holds a %q block: want CERTIFICATE blocks only, leaf first", flag, blk.Type), nil
		}
		if len(blk.Bytes) > agent.MaxDecisionCertDER {
			return nil, fmt.Sprintf("%s: certificate %d is larger than %d bytes", flag, len(der)+1, agent.MaxDecisionCertDER), nil
		}
		der = append(der, bytes.Clone(blk.Bytes))
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Sprintf("%s holds data that is not a PEM certificate block", flag), nil
	}
	if len(der) == 0 {
		return nil, fmt.Sprintf("%s holds no certificate", flag), nil
	}
	for i, d := range der {
		if _, err := x509.ParseCertificate(d); err != nil {
			return nil, fmt.Sprintf("%s: certificate %d does not parse", flag, i+1), nil
		}
	}
	return der, "", nil
}
