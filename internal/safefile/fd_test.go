//go:build unix

package safefile

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// pipeWith returns the path of a descriptor of this process that holds s, in the form a process
// substitution expands to, and keeps it open for the test.
func pipeWith(t *testing.T, s string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(s); err != nil {
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { r.Close() })
	dir := "/dev/fd/"
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no /dev/fd on this system")
	}
	return dir + strconv.Itoa(int(r.Fd()))
}

// Two flags each given a process substitution read both streams, since a descriptor is a stream the
// user chose and neither is standard input.
func TestReadDescriptorPaths(t *testing.T) {
	a, b := pipeWith(t, "first"), pipeWith(t, "second")
	if got, err := ReadOrStdin("-pubkey", a, 64); err != nil || string(got) != "first" {
		t.Errorf("first = %q, %v", got, err)
	}
	if got, err := ReadOrStdin("-stop-policy", b, 64); err != nil || string(got) != "second" {
		t.Errorf("second = %q, %v", got, err)
	}
	// The size limit applies to a stream too.
	_, err := Read(pipeWith(t, "123456"), 5, nil)
	refusal(t, err, TooLarge)
	if runtime.GOOS == "linux" {
		r, w, _ := os.Pipe()
		w.WriteString("proc")
		w.Close()
		defer r.Close()
		if got, err := Read("/proc/self/fd/"+strconv.Itoa(int(r.Fd())), 64, nil); err != nil || string(got) != "proc" {
			t.Errorf("/proc/self/fd = %q, %v", got, err)
		}
	}
}

// A symbolic link to a descriptor, a path that resolves to one, and a descriptor path with anything
// else in it are not the exact form and are refused as the pipes they are.
func TestReadDescriptorFormIsExact(t *testing.T) {
	p := pipeWith(t, "x")
	link := filepath.Join(t.TempDir(), "l")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	_, err := Read(link, 64, nil)
	refusal(t, err, NotRegular)
	for _, bad := range []string{p + "/", "/dev/fd/../fd/" + strings.TrimPrefix(p, "/dev/fd/"), "/dev//fd/" + strings.TrimPrefix(p, "/dev/fd/"), "/dev/fd/x"} {
		if _, err := Read(bad, 64, nil); err == nil {
			t.Errorf("Read(%q) succeeded", bad)
		} else if isDescriptorPath(bad) {
			t.Errorf("%q matched the descriptor form", bad)
		}
	}
}

// /dev/stdin and descriptor 0 are standard input, owned like "-": a second flag is refused.
func TestStdinPathsOwnStdin(t *testing.T) {
	defer func() { Stdin = os.Stdin; ResetStdin() }()
	for _, path := range []string{"/dev/stdin", "/dev/fd/0"} {
		ResetStdin()
		Stdin = strings.NewReader("key")
		if got, err := ReadOrStdin("-pubkey", path, 64); err != nil || string(got) != "key" {
			t.Errorf("%s = %q, %v", path, got, err)
		}
		if _, err := ReadOrStdin("-stop-policy", "-", 64); err == nil || !strings.Contains(err.Error(), "both read standard input") {
			t.Errorf("%s then - = %v, want the conflict", path, err)
		}
		// Read, which has no flag, takes the path as the owner's name.
		ResetStdin()
		Stdin = strings.NewReader("key")
		if got, err := Read(path, 64, nil); err != nil || string(got) != "key" {
			t.Errorf("Read(%s) = %q, %v", path, got, err)
		}
	}
}
