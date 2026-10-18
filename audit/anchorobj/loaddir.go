package anchorobj

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// MaxObjectBytes bounds what is read for one object: a head is a few hundred bytes plus the run ID, and
// no object an anchor writes is near this size.
const MaxObjectBytes = 1 << 20

// LoadDir reads a downloaded set of anchored objects (for example the output of "aws s3 sync" or
// "gcloud storage cp -r" of the anchor prefix) and verifies each against v. The set is every regular file
// under dir whose path ends in <partition>/<object name>; any other file, a symbolic link, a file larger
// than MaxObjectBytes and an object that does not decode under l are errors wrapping l.ErrObject (and
// l.ErrBadSignature for a signature that does not verify), so a stray or renamed file is not ignored. It
// needs no network. It returns the heads sorted by partition, then size.
//
// A directory with no heads is an error wrapping errNotFound: nothing to verify must never read as
// verified. Two validly signed heads of one tree and size with different roots in one partition are an
// error wrapping errEquivocation (CheckEquivocation, per partition, since a partition is one tree).
// LoadDir cannot show the objects' lock state (the files carry none), and cannot show that the directory
// is complete: it shows only what was exported, so compare its newest head with the live store.
func (l Layout) LoadDir(dir string, v audit.Verifier, errNotFound, errEquivocation error) ([]audit.SignedTreeHead, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("anchorobj: LoadDir needs a Verifier: %w", agent.ErrConfig)
	}
	if errNotFound == nil || errEquivocation == nil {
		return nil, fmt.Errorf("anchorobj: LoadDir needs the not-found and equivocation errors to wrap: %w", agent.ErrConfig)
	}
	type item struct {
		partition string
		sth       audit.SignedTreeHead
	}
	var items []item
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", l.ErrObject, path)
		}
		partition, name := filepath.Base(filepath.Dir(path)), d.Name()
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		body, err := io.ReadAll(io.LimitReader(f, MaxObjectBytes+1))
		if err != nil {
			return err
		}
		if len(body) > MaxObjectBytes {
			return fmt.Errorf("%w: %s is larger than %d bytes", l.ErrObject, path, MaxObjectBytes)
		}
		sth, err := l.Decode(partition, name, body, v)
		if err != nil {
			return err
		}
		items = append(items, item{partition, sth})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w in %s: nothing to verify", errNotFound, dir)
	}
	slices.SortStableFunc(items, func(a, b item) int {
		if c := strings.Compare(a.partition, b.partition); c != 0 {
			return c
		}
		return a.sth.Size - b.sth.Size
	})
	heads := make([]audit.SignedTreeHead, len(items))
	for i, it := range items {
		heads[i] = it.sth
	}
	for start := 0; start < len(items); {
		end := start
		for end < len(items) && items[end].partition == items[start].partition {
			end++
		}
		if err := CheckEquivocation(heads[start:end], errEquivocation); err != nil {
			return nil, err
		}
		start = end
	}
	return heads, nil
}
