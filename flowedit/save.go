package flowedit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Preview is what the editor shows before it writes: the plain unified diff of the bytes it would
// write against the file on disk now, the comments that diff removes, what the save needs the
// author to confirm, and whether the file changed on disk since the page read it.
type Preview struct {
	// Diff is the unified diff from the file on disk to the text, "" when they are the same.
	Diff string `json:"diff"`
	// RemovedComments are the comments the diff removes, so a dropped comment is not lost in a long
	// diff.
	RemovedComments []string `json:"removed_comments,omitempty"`
	// Changed reports the text differs from the file on disk.
	Changed bool `json:"changed"`
	// Hash is the SHA-256 of the file on disk now, and Conflict reports it is not the hash the page
	// read: the file changed behind the editor's back, and a save would be refused.
	Hash     string `json:"hash"`
	Conflict bool   `json:"conflict,omitempty"`
	// NeedsConfirmation lists what the author must confirm for a save to be accepted: each way the
	// text is weaker than the baseline, in the classifier's words, and, when the two could not be
	// compared, that. Empty for a save that needs none. A save carries this list back as it was, so
	// a confirmation of an older text does not cover a newer one.
	NeedsConfirmation []string `json:"needs_confirmation,omitempty"`
	// Errors and Warnings count the problems `bide check` finds in the text. A file with errors can
	// be saved (a half-built flow must be holdable) and the page says so each time.
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
}

// Conflict is the error of a save over a file that changed on disk since the page read it. The
// editor never overwrites it: the page shows what is on disk against the author's text and offers
// to reload.
type Conflict struct {
	// Hash is the file on disk now, Diff its difference from the author's text.
	Hash string
	Diff string
}

// Error says the file changed on disk and was not overwritten.
func (c *Conflict) Error() string {
	return "flowedit: the file changed on disk since it was read; it was not overwritten"
}

// NeedsConfirmation is the error of a save of a weakened text that did not carry the confirmation
// of exactly the items the server computed for it.
type NeedsConfirmation struct{ Items []string }

// Error lists the items the save needs confirmed.
func (n *NeedsConfirmation) Error() string {
	return "flowedit: this save weakens the policy and was not confirmed: " + strings.Join(n.Items, "; ")
}

// confirmations is what a save of text must confirm: the ways it is weaker than the baseline, and a
// failure to compare.
func (s *Server) confirmations(text []byte) []string {
	w := Compare(s.name, s.baselineSource, s.baseline, text)
	items := slices.Clone(w.Weakened)
	if w.CompareError != "" {
		items = append(items, "not compared with "+w.Baseline+": "+w.CompareError)
	}
	return items
}

// Preview describes saving text over the file whose bytes the page read as hash.
func (s *Server) Preview(text []byte, hash string) (*Preview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	disk, err := readConfig(s.path)
	if err != nil {
		return nil, err
	}
	v := BuildView(s.name, text)
	p := &Preview{
		Diff:              UnifiedDiff(filepath.Base(s.name), disk, text),
		RemovedComments:   RemovedComments(disk, text),
		Changed:           !bytes.Equal(disk, text),
		Hash:              hashOf(disk),
		NeedsConfirmation: s.confirmations(text),
		Errors:            v.Errors,
		Warnings:          v.Warnings,
	}
	p.Conflict = p.Hash != hash
	if v.ParseError != "" {
		p.Errors++ // a text that does not parse has one error the counts do not hold
	}
	return p, nil
}

// Save writes text over the file, if and only if: the file on disk is still the bytes the page read
// (hash), the text is a config the editor will write (at most MaxFileSize bytes, no NUL), and, when
// the text is weaker than the baseline or could not be compared, confirmed is exactly the list
// Preview named. The write is atomic: the text goes to a temporary file in the same directory,
// which replaces the file by a rename, so a reader sees the old file or the new one and a failure
// leaves the old one. It returns the file as it is after.
//
// There is a small window between the check and the rename in which another process could write the
// file; the rename then replaces that write. The check is compare-and-swap for a person editing in
// two places, not a lock.
func (s *Server) Save(text []byte, hash string, confirmed []string) (*Doc, error) {
	if len(text) > MaxFileSize {
		return nil, fmt.Errorf("flowedit: the text is larger than %d bytes", MaxFileSize)
	}
	if bytes.IndexByte(text, 0) >= 0 {
		return nil, errors.New("flowedit: the text holds a NUL byte")
	}
	// One save at a time: the hash check and the write are one step for every request this server
	// answers (two tabs, a double click, a retry), so of several saves over the same hash exactly one
	// succeeds and the others are conflicts.
	s.mu.Lock()
	defer s.mu.Unlock()
	disk, err := readConfig(s.path)
	if err != nil {
		return nil, err
	}
	if hashOf(disk) != hash {
		return nil, &Conflict{Hash: hashOf(disk), Diff: UnifiedDiff(filepath.Base(s.name), disk, text)}
	}
	if items := s.confirmations(text); len(items) > 0 && !slices.Equal(confirmed, items) {
		return nil, &NeedsConfirmation{Items: items}
	}
	if !bytes.Equal(disk, text) {
		if err := s.write(text, hash); err != nil {
			return nil, err
		}
	}
	return s.docLocked()
}

// fileSystem is the part of the file system a save uses, as a value so that a test can make the
// write or the rename fail the way a full disk or a permission does.
type fileSystem struct {
	createTemp func(dir, pattern string) (*os.File, error)
	write      func(f *os.File, b []byte) error
	rename     func(oldpath, newpath string) error
}

var osFS = fileSystem{
	createTemp: os.CreateTemp,
	write:      func(f *os.File, b []byte) error { _, err := f.Write(b); return err },
	rename:     os.Rename,
}

// write replaces the file with text by an atomic rename of a temporary file in its directory. The
// target is checked again here, not only at startup: a path that has become a symbolic link, or a
// directory that is no longer the one resolved at startup, is refused.
func (s *Server) write(text []byte, hash string) (err error) {
	fi, err := os.Lstat(s.path)
	if err != nil {
		return fmt.Errorf("flowedit: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return fmt.Errorf("flowedit: %s is no longer the regular file it was at startup; nothing was written", s.path)
	}
	// What a rename over the file would silently change is refused instead: a file its owner made
	// read-only is not replaced (a text editor would refuse it too), and a file with other names
	// would be left behind as a hard link to the old content.
	if fi.Mode().Perm()&0o200 == 0 {
		return fmt.Errorf("flowedit: %s is read-only (mode %v); the editor does not replace a read-only file, so nothing was written", s.path, fi.Mode().Perm())
	}
	if n := linkCount(fi); n > 1 {
		return fmt.Errorf("flowedit: %s has %d names (hard links); a save replaces the file and would leave the others with the old text, so nothing was written", s.path, n)
	}
	dir := filepath.Dir(s.path)
	if real, err := filepath.EvalSymlinks(dir); err != nil || real != dir {
		return fmt.Errorf("flowedit: the directory of %s is no longer the one resolved at startup; nothing was written", s.path)
	}
	tmp, err := s.fs.createTemp(dir, ".bide-edit-*")
	if err != nil {
		return fmt.Errorf("flowedit: cannot write beside %s: %w", s.path, err)
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := s.fs.write(tmp, text); err != nil {
		return fmt.Errorf("flowedit: writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Chmod(fi.Mode().Perm()); err != nil && !isUnsupported(err) {
		return fmt.Errorf("flowedit: %w", err)
	}
	carryOwner(tmp, fi)
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("flowedit: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flowedit: %w", err)
	}
	// The hash is checked again just before the rename, so the window another process has is the
	// rename's own.
	if cur, rerr := readConfig(s.path); rerr != nil || hashOf(cur) != hash {
		if rerr != nil {
			return rerr
		}
		return &Conflict{Hash: hashOf(cur), Diff: UnifiedDiff(filepath.Base(s.name), cur, text)}
	}
	if err := s.fs.rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("flowedit: replacing %s: %w", s.path, err)
	}
	syncDir(dir)
	return nil
}

// isUnsupported reports an error from a file system that has no such operation (Chmod on Windows).
func isUnsupported(err error) bool { return errors.Is(err, errors.ErrUnsupported) }

// syncDir makes the rename durable where the platform allows; a failure is not one of the save.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
