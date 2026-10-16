package flowedit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
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
}

// Compare classifies text against baseline, both the contents of a config file called name.
func Compare(name, source string, baseline, text []byte) *Weakening {
	w := &Weakening{Baseline: source}
	cfg := func(b []byte) (runview.Config, error) {
		data := b
		if formatOf(name) == "yaml" {
			var err error
			if data, err = ToJSON(b); err != nil {
				return runview.Config{}, err
			}
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
	for _, c := range d.Changes {
		w.Changes = append(w.Changes, c.String())
	}
	w.Weakened, w.Strengthened = d.Weakened, d.Strengthened
	if len(d.Weakened) > 0 {
		w.Word, w.Line = d.Verdict.Word, d.Verdict.Line
	}
	return w
}

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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "show", rev+":./"+filepath.ToSlash(name))
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
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
