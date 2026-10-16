package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Markers on a CRL archived but not added to the snapshot: a rollback (its number is below the
// issuer's high-water mark) or a conflict (the mark's number with another TBSCertList). Load never seeds
// them; an auditor reads them as what the CRL source served that the series does not contain.
const (
	MarkerRollback = "rollback"
	MarkerConflict = "conflict"
	// MarkerSetAside marks a CRL a source served after an operator set it aside.
	MarkerSetAside = "setaside"
)

// ArchivedCRL describes one CRL file of the archive. The number and time come from the file name and
// are not trusted: Load verifies a CRL before it takes anything from it.
type ArchivedCRL struct {
	Name       string
	Number     *big.Int
	ThisUpdate time.Time
	Marker     string
	// Source and Fetched record where and when the refresher got it (provenance, never evidence).
	Source  string
	Fetched time.Time
}

// ResetRecord is the durable record of an operator's reset of an issuer's high-water mark: the CRLs
// it sets aside, who did it and why. Load computes the mark over the archive without the CRLs a record
// names. The CRLs stay in the archive and are listed for audit.
type ResetRecord struct {
	Name     string      `json:"-"`
	Issuer   string      `json:"issuer"`
	SetAside []ResetItem `json:"set_aside"`
	// LostSerials are the certificate serials (decimal) the CRLs set aside listed and no CRL left in the
	// snapshot lists: the revocations the reset dropped from the live gate, for an auditor.
	LostSerials []string  `json:"lost_serials,omitempty"`
	Reason      string    `json:"reason"`
	Operator    string    `json:"operator"`
	Time        time.Time `json:"time"`
	Acked       bool      `json:"-"`
}

// ResetItem is one CRL a reset sets aside.
type ResetItem struct {
	Hash   string `json:"hash"`
	Number string `json:"number"`
}

// CRLArchive is the dated archive of the CRLs the refresher accepts, and its rollback memory. Put must be
// durable and idempotent by hash.
type CRLArchive interface {
	// PutCRL stores der for issuer. meta.Marker is empty for a CRL of the series.
	PutCRL(ctx context.Context, issuer SPKIHash, a ArchivedCRL, der []byte) error
	// List returns the issuer's CRL files, newest number first.
	List(ctx context.Context, issuer SPKIHash) ([]ArchivedCRL, error)
	// Read returns the bytes of a listed file.
	Read(ctx context.Context, issuer SPKIHash, a ArchivedCRL) ([]byte, error)
	// PutReset appends a reset record; Resets lists them (with Acked set) oldest first; AckReset marks
	// one acknowledged by an operator.
	PutReset(ctx context.Context, issuer SPKIHash, r ResetRecord) error
	Resets(ctx context.Context, issuer SPKIHash) ([]ResetRecord, error)
	AckReset(ctx context.Context, issuer SPKIHash, r ResetRecord) error
}

// DirCRLArchive is a CRLArchive in a directory: <dir>/crl/<issuer key hash>/<number>-<thisUpdate>.crl, with
// a .meta.json beside each file (source and fetch time), and RESET-<time>.json for resets. A file is
// written to a temporary name, synced, renamed, and the directory synced, so a reader sees all of it or
// none; names derive from the content, so several replicas may share the directory. The directory is
// created 0700 and an existing one that is writable by others, or on Unix not owned by the process user,
// is refused: the archive is the refresher's rollback memory.
type DirCRLArchive string

func (d DirCRLArchive) issuerDir(issuer SPKIHash) string {
	return filepath.Join(string(d), "crl", hex.EncodeToString(issuer[:]))
}

const archiveTimeFmt = "20060102T150405Z"

func (d DirCRLArchive) prepare(issuer SPKIHash) (string, error) {
	dir := d.issuerDir(issuer)
	for _, p := range []string{string(d), filepath.Join(string(d), "crl"), dir} {
		if err := ensureArchiveDir(p); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func fileName(a ArchivedCRL, hash [32]byte) string {
	n := fmt.Sprintf("%s-%s", a.Number, a.ThisUpdate.UTC().Format(archiveTimeFmt))
	if a.Marker != "" {
		n += fmt.Sprintf(".%s-%x", a.Marker, hash[:4])
	}
	return n + ".crl"
}

// PutCRL implements CRLArchive.
func (d DirCRLArchive) PutCRL(_ context.Context, issuer SPKIHash, a ArchivedCRL, der []byte) error {
	dir, err := d.prepare(issuer)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(der)
	name := fileName(a, hash)
	if a.Marker == "" {
		// Two different CRLs under one series name would be a conflict; the caller marks those. A
		// file that is already there must hold these bytes.
		if old, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			// The same CRL re-signed (same TBSCertList, other signature bytes) is already archived.
			have, ok1 := tbsHash(old)
			want, ok2 := tbsHash(der)
			if sha256.Sum256(old) == hash || (ok1 && ok2 && have == want) {
				return nil
			}
			return fmt.Errorf("audit: CRL archive: %s exists with a different TBSCertList", name)
		}
	}
	meta, _ := json.Marshal(struct {
		Source  string    `json:"source"`
		Fetched time.Time `json:"fetched"`
		SHA256  string    `json:"sha256"`
	}{a.Source, a.Fetched.UTC(), hex.EncodeToString(hash[:])})
	if err := writeFileAtomic(dir, name+".meta.json", meta, 0o644); err != nil {
		return err
	}
	return writeFileAtomic(dir, name, der, 0o644)
}

// List implements CRLArchive.
func (d DirCRLArchive) List(_ context.Context, issuer SPKIHash) ([]ArchivedCRL, error) {
	ents, err := os.ReadDir(d.issuerDir(issuer))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ArchivedCRL
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".crl") {
			continue
		}
		a, ok := parseFileName(n)
		if !ok {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(d.issuerDir(issuer), n+".meta.json")); err == nil {
			var m struct {
				Source  string    `json:"source"`
				Fetched time.Time `json:"fetched"`
			}
			if json.Unmarshal(b, &m) == nil {
				a.Source, a.Fetched = m.Source, m.Fetched
			}
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(x, y ArchivedCRL) int {
		if c := y.Number.Cmp(x.Number); c != 0 {
			return c
		}
		return y.ThisUpdate.Compare(x.ThisUpdate)
	})
	return out, nil
}

func parseFileName(n string) (ArchivedCRL, bool) {
	base := strings.TrimSuffix(n, ".crl")
	a := ArchivedCRL{Name: n}
	if i := strings.IndexByte(base, '.'); i >= 0 {
		m := base[i+1:]
		base = base[:i]
		if j := strings.IndexByte(m, '-'); j > 0 {
			a.Marker = m[:j]
		} else {
			return a, false
		}
	}
	num, ts, ok := strings.Cut(base, "-")
	if !ok {
		return a, false
	}
	a.Number, ok = new(big.Int).SetString(num, 10)
	if !ok || a.Number.Sign() < 0 {
		return a, false
	}
	t, err := time.Parse(archiveTimeFmt, ts)
	if err != nil {
		return a, false
	}
	a.ThisUpdate = t
	return a, true
}

// Read implements CRLArchive.
func (d DirCRLArchive) Read(_ context.Context, issuer SPKIHash, a ArchivedCRL) ([]byte, error) {
	if a.Name != filepath.Base(a.Name) {
		return nil, fmt.Errorf("audit: CRL archive: bad file name %q", a.Name)
	}
	return os.ReadFile(filepath.Join(d.issuerDir(issuer), a.Name))
}

// PutReset implements CRLArchive.
func (d DirCRLArchive) PutReset(_ context.Context, issuer SPKIHash, r ResetRecord) error {
	dir, err := d.prepare(issuer)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	name := "RESET-" + r.Time.UTC().Format(archiveTimeFmt) + ".json"
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		return fmt.Errorf("audit: CRL archive: %s exists; resets are append-only", name)
	}
	return writeFileAtomic(dir, name, b, 0o644)
}

// Resets implements CRLArchive.
func (d DirCRLArchive) Resets(_ context.Context, issuer SPKIHash) ([]ResetRecord, error) {
	dir := d.issuerDir(issuer)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ResetRecord
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "RESET-") || !strings.HasSuffix(n, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		var r ResetRecord
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("audit: CRL archive: %s: %w", n, err)
		}
		r.Name = n
		_, serr := os.Stat(filepath.Join(dir, n+".ack"))
		r.Acked = serr == nil
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b ResetRecord) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// AckReset implements CRLArchive.
func (d DirCRLArchive) AckReset(_ context.Context, issuer SPKIHash, r ResetRecord) error {
	if r.Name != filepath.Base(r.Name) || !strings.HasPrefix(r.Name, "RESET-") {
		return fmt.Errorf("audit: CRL archive: bad reset name %q", r.Name)
	}
	return writeFileAtomic(d.issuerDir(issuer), r.Name+".ack", []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// writeFileAtomic writes data to dir/name through a temporary file: write, sync, rename, sync the
// directory.
func writeFileAtomic(dir, name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tn := tmp.Name()
	defer os.Remove(tn)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil && !isChmodUnsupported(err) {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tn, filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

// ensureArchiveDir creates dir 0700 or checks an existing one.
func ensureArchiveDir(dir string) error {
	fi, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("audit: CRL archive: %w", err)
		}
		fi, err = os.Stat(dir)
	}
	if err != nil {
		return fmt.Errorf("audit: CRL archive: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("audit: CRL archive: %s is not a directory: %w", dir, agent.ErrConfig)
	}
	if err := checkArchiveDirOwner(fi); err != nil {
		return fmt.Errorf("audit: CRL archive: %s: %v: %w", dir, err, agent.ErrConfig)
	}
	return nil
}
