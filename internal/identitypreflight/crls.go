package identitypreflight

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/crlrefresh"
	"github.com/bide-ai/bide/internal/safefile"
)

// readOnlyArchive reads a crlrefresh archive directory and writes nothing: a pre-flight must not change
// the archive a fetch-crl job keeps (its rollback memory). What the refresher would store is dropped; what
// it fetched stays in the snapshot for the checks.
type readOnlyArchive struct{ crlrefresh.DirArchive }

func (readOnlyArchive) PutCRL(context.Context, audit.SPKIHash, crlrefresh.ArchivedCRL, []byte) error {
	return nil
}
func (readOnlyArchive) PutReset(context.Context, audit.SPKIHash, crlrefresh.ResetRecord) error {
	return nil
}
func (readOnlyArchive) AckReset(context.Context, audit.SPKIHash, crlrefresh.ResetRecord) error {
	return errors.New("the pre-flight reads the archive and does not acknowledge resets")
}

// isArchive reports whether dir holds a crlrefresh archive (a crl subdirectory), as opposed to a flat
// directory of CRL files.
func isArchive(dir string) (bool, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return false, &IOError{Err: fmt.Errorf("-crl-dir: %w", err)}
	}
	if !fi.IsDir() {
		return false, &IOError{Err: fmt.Errorf("-crl-dir %s is not a directory", dir)}
	}
	if sub, err := os.Stat(filepath.Join(dir, "crl")); err == nil && sub.IsDir() {
		return true, nil
	}
	return false, nil
}

const maxFlatFiles = 4096

// loadFlat adds the CRLs in the regular files of dir (*.crl, *.der, *.pem; DER or PEM "X509 CRL") to the
// snapshot, which verifies each against its pinned issuer: a file that does not verify is reported, never
// trusted. It returns the number loaded and a message per rejected file.
// afterListing is a test hook run between a CRL file's type check and its open, so a test can swap
// the file there. It is nil outside tests.
var afterListing func()

func loadFlat(dir string, snap *audit.StatusSnapshot) (loaded int, rejected []string, err error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil, &IOError{Err: fmt.Errorf("-crl-dir: %w", err)}
	}
	maxBytes, _, _ := snap.Limits()
	seen := 0
	for _, e := range ents {
		n := e.Name()
		ext := strings.ToLower(filepath.Ext(n))
		if ext != ".crl" && ext != ".der" && ext != ".pem" {
			continue
		}
		if !e.Type().IsRegular() {
			rejected = append(rejected, fmt.Sprintf("%s is not a regular file (a directory or a link): not read", n))
			continue
		}
		if seen++; seen > maxFlatFiles {
			rejected = append(rejected, fmt.Sprintf("more than %d CRL files: the rest are not read", maxFlatFiles))
			break
		}
		f, err := safefile.OpenRegular(filepath.Join(dir, n), afterListing)
		if err != nil {
			return loaded, rejected, &IOError{Err: fmt.Errorf("-crl-dir: %w", err)}
		}
		b, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
		f.Close()
		if err != nil {
			return loaded, rejected, &IOError{Err: fmt.Errorf("-crl-dir: %s: %w", n, err)}
		}
		ders := [][]byte{b}
		if blk, _ := pem.Decode(b); blk != nil {
			ders = nil
			for rest := b; ; {
				var blk *pem.Block
				blk, rest = pem.Decode(rest)
				if blk == nil {
					break
				}
				if blk.Type == "X509 CRL" {
					ders = append(ders, blk.Bytes)
				}
			}
		}
		if len(ders) == 0 {
			rejected = append(rejected, fmt.Sprintf("%s holds no CRL", n))
		}
		for _, der := range ders {
			if _, err := snap.AddCRL(der); err != nil {
				rejected = append(rejected, fmt.Sprintf("%s: %v", n, err))
				continue
			}
			loaded++
		}
	}
	return loaded, rejected, nil
}

// securityEvents are the refresher events an operator must see even when the issuer ends up fresh.
var securityEvents = []crlrefresh.EventKind{crlrefresh.EventRollback, crlrefresh.EventConflict, crlrefresh.EventBadSignature,
	crlrefresh.EventDistrusted, crlrefresh.EventUnsupportedCRL, crlrefresh.EventFutureDated, crlrefresh.EventStaleOnArrival,
	crlrefresh.EventOversizeResponse, crlrefresh.EventRedirectRefused, crlrefresh.EventArchiveCorrupt, crlrefresh.EventSetAsideServed}

func isSecurity(k crlrefresh.EventKind) bool { return slices.Contains(securityEvents, k) }
