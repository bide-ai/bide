// Package browsertest runs the Node scripts that drive a page in Chrome for the packages' tests. It is
// for tests only. A script that stalls (a Chrome that does not start, a page that never settles) would
// otherwise hold the test until the whole test binary times out, with nothing said about where it
// stopped: Run gives it a deadline, stops the script and the Chrome it started, and returns what the
// script had printed, stdout and stderr.
//
// Chrome is not in the script's process group: puppeteer starts it detached. So the run owns the
// Chrome profile: it makes a fresh directory, hands it to the script in BIDE_CHROME_PROFILE (every
// script launches with userDataDir: env.BIDE_CHROME_PROFILE), and after the script ends, however it
// ended, kills every process whose command line names that directory and removes it.
package browsertest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Deadline is how long a script may run. A script under a loaded machine takes up to four times its
// usual few seconds; five minutes is far past that and well short of the test binary's timeout.
var Deadline = 5 * time.Minute

// ProfileEnv is the environment variable that carries the Chrome profile directory to the script.
const ProfileEnv = "BIDE_CHROME_PROFILE"

// DeadlineError is a script that was still running at its deadline and was stopped.
type DeadlineError struct {
	Script   string
	Deadline time.Duration
	Stderr   string // the tail of what it wrote to standard error
	Stdout   string // the tail of what it wrote to standard output
	Profile  string // the Chrome profile directory the run owned
}

func (e *DeadlineError) Error() string {
	return fmt.Sprintf("%s was still running after %v and was stopped with the processes it started (Chrome profile %s)\nstderr: %s\nstdout: %s", e.Script, e.Deadline, e.Profile, e.Stderr, e.Stdout)
}

// TimedOut reports whether err is a script stopped at its deadline. A caller that fails on it should
// report it first: a script cut off has printed no result, and "no result" is the consequence.
func TimedOut(err error) bool {
	var d *DeadlineError
	return errors.As(err, &d)
}

// Run runs node with args in dir, with env added to the environment of the test, and returns its
// standard output. When the script outlives Deadline it is stopped (see RunTo) and the error is a
// *DeadlineError quoting the tail of what the script wrote.
func Run(node, dir string, env []string, args ...string) ([]byte, error) {
	return RunFor(Deadline, node, dir, env, args...)
}

// RunFor is Run with a deadline given.
func RunFor(d time.Duration, node, dir string, env []string, args ...string) ([]byte, error) {
	return RunTo(d, nil, node, dir, env, args...)
}

// RunTo is RunFor that also copies the script's standard error to w as it is written (nil: not copied),
// for a script whose progress a person watches; the error of a failed run still quotes its tail.
//
// At the deadline the script's process group gets SIGTERM, which puppeteer answers by closing Chrome;
// two seconds later the group is killed. A script whose event loop is stuck cannot answer, which is
// the usual way to stall, so the second layer does not depend on it: whatever the script did, every
// process whose command line holds the run's profile directory is killed, and the directory removed.
func RunTo(d time.Duration, w io.Writer, node, dir string, env []string, args ...string) ([]byte, error) {
	profile, err := newProfile()
	if err != nil {
		return nil, err
	}
	defer func() {
		killProfile(profile)
		os.RemoveAll(profile)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string(nil), env...), ProfileEnv+"="+profile)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if w != nil {
		cmd.Stderr = io.MultiWriter(&stderr, w)
	}
	group(cmd)
	err = cmd.Run()
	if cmd.Process != nil {
		killGroup(cmd.Process.Pid)
	}
	if ctx.Err() != nil {
		return stdout.Bytes(), &DeadlineError{
			Script: node + " " + strings.Join(args, " "), Deadline: d, Profile: profile,
			Stderr: tail(stderr.Bytes()), Stdout: tail(stdout.Bytes()),
		}
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%w\nstderr: %s", err, tail(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// randRead is the source of the names; a test replaces it to make two runs draw the same one.
var randRead = rand.Read

// newProfile makes the run's Chrome profile directory under the temporary directory. Its name has a
// fixed length (16 random hex digits), and os.Mkdir fails if it exists, so no run's directory is a prefix
// of another's: killing the processes of one run, which matches the directory in their command lines,
// can never take another run's Chrome with it.
func newProfile() (string, error) {
	for i := 0; i < 10; i++ {
		var r [8]byte
		if _, err := randRead(r[:]); err != nil {
			return "", err
		}
		dir := filepath.Join(os.TempDir(), "bide-chrome-"+hex.EncodeToString(r[:]))
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("browsertest: no unused profile directory name after 10 tries")
}

// tail is the last 2 KiB of b, so a long log does not bury the line that matters.
func tail(b []byte) string {
	const max = 2048
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return strings.TrimSpace(string(b))
}
