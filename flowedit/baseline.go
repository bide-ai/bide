package flowedit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/internal/runview"
)

// Weakening is the answer to "is this text a weaker flow than the one we started from": the
// classifier `bide check -diff` runs (runview.BuildDiff), on the text and the baseline. The editor
// computes nothing of its own here: when the classifier gains a category, this carries it.
type Weakening struct {
	// Baseline says what the text is compared with: "git HEAD" or the revision -base names, or "the
	// file as it was when the editor opened it".
	Baseline string `json:"baseline"`
	// Weakened lists each way the text is weaker than the baseline, in the classifier's words
	// ("charge: approval 2 of 3 -> 1 of 3"); empty when it is not weaker. The page shows them in a
	// banner that stays until the text no longer weakens.
	Weakened []string `json:"weakened,omitempty"`
	// Word and Line are the banner's heading and sentence (APPROVAL GATE WEAKENED, SAFETY CHECK
	// WEAKENED), set only with Weakened.
	Word string `json:"word,omitempty"`
	Line string `json:"line,omitempty"`
	// Strengthened lists each way it is stronger.
	Strengthened []string `json:"strengthened,omitempty"`
	// Changes lists every change from the baseline, one line each, in the classifier's words.
	Changes []string `json:"changes,omitempty"`
	// CompareError is set when the two could not be compared (the text does not parse): nothing can
	// be said about weakening, which is not the same as none, so a save names it.
	CompareError string `json:"compare_error,omitempty"`
	// NotCertified is set in typed mode when the text, or the baseline, has errors against the
	// program's blocks, so the typed comparison had nothing sound to read: the comparison was made
	// structurally, as the structural editor makes it, and what the blocks declare in Go is not known.
	// A save names it and needs the same confirmation a weakening needs, so a typed editor is never
	// less strict than a structural one.
	NotCertified string `json:"not_certified,omitempty"`
}

// Compare classifies text against baseline, both the contents of a config file called name.
func Compare(name, source string, baseline, text []byte) *Weakening {
	return compare(name, source, baseline, text, nil)
}

// compare is Compare for an editor that may know the program's blocks (ty not nil). The program the
// baseline was written for is not the one the editor knows, so the editor's verdict is never only the
// typed one: it is the typed comparison (baseline and text read with the blocks as they are now, which
// knows the gates the blocks declare in Go) together with the structural comparison, the one the
// structural editor makes, and a weakening either reports is reported. A block swapped away from one
// whose gate changed in Go since the baseline is therefore still a possible weakening that needs
// confirming, as it is without the blocks. Typed alone is not enough, and a typed text read against a
// structural baseline reads every safety the baseline left to its block as lowered; the union keeps
// the strictness of the one and the precision of the other.
func compare(name, source string, baseline, text []byte, ty *typing) *Weakening {
	w := compareWith(name, source, baseline, text, ty)
	if ty == nil {
		return w
	}
	s := compareWith(name, source, baseline, text, nil)
	if w.CompareError != "" || s.CompareError != "" {
		return w
	}
	for _, x := range s.Weakened {
		if !slices.Contains(w.Weakened, x) {
			w.Weakened = append(w.Weakened, x)
		}
	}
	if len(w.Weakened) > 0 && w.Word == "" {
		w.Word, w.Line = s.Word, s.Line
	}
	return w
}

func compareWith(name, source string, baseline, text []byte, ty *typing) *Weakening {
	w := &Weakening{Baseline: source}
	cfg := func(b []byte) (runview.Config, error) {
		data := b
		if formatOf(name) == "yaml" {
			var err error
			if data, err = ToJSON(b); err != nil {
				return runview.Config{}, err
			}
		}
		if ty != nil {
			return ty.config(name, data), nil
		}
		return runview.Config{Name: name, Data: data}, nil
	}
	old, err := cfg(baseline)
	if err != nil {
		w.CompareError = "the baseline does not parse: " + err.Error()
		return w
	}
	cur, err := cfg(text)
	if err != nil {
		w.CompareError = "the text does not parse: " + err.Error()
		return w
	}
	d, err := runview.BuildDiff(runview.Diff{Old: old, New: cur})
	if err != nil {
		w.CompareError = err.Error()
		return w
	}
	w.NotCertified = d.Fallback
	for _, c := range d.Changes {
		w.Changes = append(w.Changes, c.String())
	}
	w.Weakened, w.Strengthened = d.Weakened, d.Strengthened
	if len(d.Weakened) > 0 {
		w.Word, w.Line = d.Verdict.Word, d.Verdict.Line
	}
	return w
}

// compare is Compare with what this editor knows of the program.
func (s *Server) compare(text []byte) *Weakening {
	return compare(s.name, s.baselineSource, s.baseline, text, s.typing)
}

// gitTimeout is how long gitShow waits for git: a generous bound for a local `git show`, there so that a git that
// hangs (a stalled network file system, a credential helper that waits) cannot hang the editor's start. A test sets it, so no test that does may run in parallel.
var gitTimeout = 15 * time.Second

// gitShow reads name as it is at rev in the git working tree that holds dir: `git show
// rev:./name`, run in dir. It returns an error when git is not installed, dir is not in a
// repository, or the file is not at that revision.
func gitShow(dir, rev, name string) ([]byte, error) {
	if rev == "" || strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, ":\x00\n") {
		return nil, fmt.Errorf("%q is not a git revision", rev)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, errors.New("git is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "-c", "core.autocrlf=false", "-c", "core.eol=lf", "show", rev+":./"+filepath.ToSlash(name))
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	// A git killed at the deadline can leave a child (a helper it started) holding the pipes; do not wait for it.
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// git answered and exited 0; a helper it left running holds the pipes. What it wrote is complete.
		err = nil
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && (cmd.ProcessState == nil || cmd.ProcessState.ExitCode() == -1) {
			// Our own deadline stopped git (it did not exit on its own), not git's answer: say so, rather than
			// the "signal: killed" the kill leaves.
			return nil, fmt.Errorf("git show %s:./%s: git did not answer within %s and was stopped (a slow or loaded machine, or a hung file system)", rev, name, gitTimeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git show %s:./%s: %s", rev, name, msg)
	}
	if len(out) > MaxFileSize {
		return nil, fmt.Errorf("git show %s:./%s: larger than %d bytes", rev, name, MaxFileSize)
	}
	return out, nil
}

// gitNotThere reports a git failure that only means the file is not in a repository's history: not a
// repository, or a path or revision git does not have. Git not installed, an ownership refusal and
// every other failure are not that.
func gitNotThere(err error) bool {
	m := err.Error()
	for _, ok := range []string{"not a git repository", "exists on disk, but not in", "does not exist in", "unknown revision or path", "bad revision", "ambiguous argument"} {
		if strings.Contains(m, ok) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
