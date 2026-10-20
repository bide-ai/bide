// Package browsertest runs the Node scripts that drive a page in Chrome for the packages' tests. It is
// for tests only. A script that stalls (a Chrome that does not start, a page that never settles) would
// otherwise hold the test until the whole test binary times out, with nothing said about where it
// stopped: Run gives it a deadline, kills the script and the Chrome it started together, and returns
// what the script had printed, stdout and stderr.
package browsertest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Deadline is how long a script may run. A script under a loaded machine takes up to four times its
// usual few seconds; five minutes is far past that and well short of the test binary's timeout.
var Deadline = 5 * time.Minute

// Run runs node with args in dir, with env added to the environment of the test, and returns its
// standard output. When the script outlives Deadline it is killed with the processes it started (the
// Chrome it launched included) and the error says so and quotes the last of what the script wrote to
// standard error and standard output.
func Run(node, dir string, env []string, args ...string) ([]byte, error) {
	return RunFor(Deadline, node, dir, env, args...)
}

// RunFor is Run with a deadline given.
func RunFor(d time.Duration, node, dir string, env []string, args ...string) ([]byte, error) {
	return RunTo(d, nil, node, dir, env, args...)
}

// RunTo is RunFor that also copies the script's standard error to w as it is written (nil: not copied),
// for a script whose progress a person watches; the error of a failed run still quotes its tail.
func RunTo(d time.Duration, w io.Writer, node, dir string, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if w != nil {
		cmd.Stderr = io.MultiWriter(&stderr, w)
	}
	group(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s was still running after %v and was killed with the processes it started\nstderr: %s\nstdout: %s",
			node, strings.Join(args, " "), d, tail(stderr.Bytes()), tail(stdout.Bytes()))
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%w\nstderr: %s", err, tail(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// tail is the last 2 KiB of b, so a long log does not bury the line that matters.
func tail(b []byte) string {
	const max = 2048
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return strings.TrimSpace(string(b))
}
