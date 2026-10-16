package flowedit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// dirFiles lists the names in dir: after a save, or a failed one, the directory holds the file and
// nothing else.
func dirFiles(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func saveBody(text, hash string, confirmed ...string) string {
	b, _ := json.Marshal(map[string]any{"text": text, "hash": hash, "confirmed": confirmed})
	return string(b)
}

func TestSaveWritesTheTextAtomically(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	edited, err := ApplyOperation("refund.yaml", []byte(weakFlow), Operation{Op: "set_approval", Node: "charge", Approval: gate(3, []string{"ops", "finance", "risk"}, true, "24h")})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Save(edited.Text, hashOf([]byte(weakFlow)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != string(edited.Text) || d.Text != got || d.Hash != hashOf(edited.Text) {
		t.Errorf("file %q, doc %q, hash %s", got, d.Text, d.Hash)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
			t.Errorf("the mode became %v", fi.Mode().Perm())
		}
	}
	if names := dirFiles(t, filepath.Dir(p)); !slices.Equal(names, []string{"refund.yaml"}) {
		t.Errorf("the directory holds %q", names)
	}
	// The hash the page holds is now the new one: a second save from it is accepted, and one from the old is a conflict.
	if _, err := s.Save(edited.Text, d.Hash, nil); err != nil {
		t.Errorf("a no-op save: %v", err)
	}
	var c *Conflict
	if _, err := s.Save([]byte(weakFlow), hashOf([]byte(weakFlow)), nil); !errors.As(err, &c) {
		t.Errorf("a save from the old hash: %v", err)
	}
}

// A file with errors is saved: a half-built flow must be holdable. The preview says it has them.
func TestSaveAcceptsAFileWithErrors(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	broken := strings.Replace(weakFlow, "need: 2", "need: 9", 1)
	pv, err := s.Preview([]byte(broken), hashOf([]byte(weakFlow)))
	if err != nil || pv.Errors == 0 {
		t.Fatalf("%v %+v", err, pv)
	}
	if _, err := s.Save([]byte(broken), pv.Hash, s.confirmations([]byte(broken))); err != nil {
		t.Fatal(err)
	}
	if readFile(t, p) != broken {
		t.Error("not written")
	}
	// A text that does not parse at all is a file too: it is saved, and the preview counts an error.
	pv, _ = s.Preview([]byte("nodes: ["), hashOf([]byte(broken)))
	if pv.Errors == 0 {
		t.Errorf("%+v", pv)
	}
}

// The file changed behind the editor: the save is refused, the file is the other writer's, and the
// answer carries what the file is now against what the author has.
func TestSaveNeverOverwritesAChangedFile(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	h := s.Handler()
	other := strings.Replace(weakFlow, "flow: refund", "flow: refund2", 1)
	if err := os.WriteFile(p, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	mine := strings.Replace(weakFlow, "need: 2", "need: 3", 1)
	w := call(h, s, "/api/save", saveBody(mine, hashOf([]byte(weakFlow))), nil)
	if w.Code != 409 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var r struct {
		Conflict bool   `json:"conflict"`
		Hash     string `json:"hash"`
		Diff     string `json:"diff"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || !r.Conflict || r.Hash != hashOf([]byte(other)) || !strings.Contains(r.Diff, "-flow: refund2") || !strings.Contains(r.Diff, "+flow: refund\n") {
		t.Errorf("%v %+v", err, r)
	}
	if readFile(t, p) != other {
		t.Error("the other writer's file was overwritten")
	}
	// The preview says so before the author tries.
	pv, _ := s.Preview([]byte(mine), hashOf([]byte(weakFlow)))
	if !pv.Conflict || pv.Hash != hashOf([]byte(other)) {
		t.Errorf("%+v", pv)
	}
	// A save from the hash the page re-read is accepted.
	if w := call(h, s, "/api/save", saveBody(mine, hashOf([]byte(other))), nil); w.Code != 200 {
		t.Errorf("after a reload: %d %s", w.Code, w.Body)
	}
}

// A weakened text is saved only with the confirmation of exactly what the server computed for it:
// the page cannot be the only thing that asks, and an old confirmation does not cover a new text.
func TestSaveOfAWeakenedTextNeedsTheConfirmation(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	h := s.Handler()
	hash := hashOf([]byte(weakFlow))
	lower := strings.Replace(weakFlow, "need: 2", "need: 1", 1)
	items := s.confirmations([]byte(lower))
	// the gate, and the assertion that held and is violated now (#329)
	if len(items) != 2 || !strings.Contains(items[0], "charge: approval 2 of 3") || !strings.Contains(items[1], "assertion no longer holds") {
		t.Fatalf("items %q", items)
	}
	// No confirmation, a partial one, a wrong one and a stale one (for another weakening): refused, 422, nothing written.
	other := strings.Replace(weakFlow, "      exclude_principal: true\n", "", 1)
	for name, confirmed := range map[string][]string{
		"none":         nil,
		"empty":        {},
		"another text": s.confirmations([]byte(other)),
		"extra":        append(slices.Clone(items), "something else"),
		"reworded":     {"charge: approval weakened"},
	} {
		w := call(h, s, "/api/save", saveBody(lower, hash, confirmed...), nil)
		if w.Code != 422 {
			t.Errorf("%s: status %d", name, w.Code)
			continue
		}
		var r struct {
			Needs []string `json:"needs_confirmation"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || !slices.Equal(r.Needs, items) {
			t.Errorf("%s: %v %q", name, err, r.Needs)
		}
		if readFile(t, p) != weakFlow {
			t.Fatalf("%s: the file was written without the confirmation", name)
		}
	}
	// The preview names the same items.
	if pv, _ := s.Preview([]byte(lower), hash); !slices.Equal(pv.NeedsConfirmation, items) {
		t.Errorf("preview %q", pv.NeedsConfirmation)
	}
	if w := call(h, s, "/api/save", saveBody(lower, hash, items...), nil); w.Code != 200 {
		t.Fatalf("confirmed: %d %s", w.Code, w.Body)
	}
	if readFile(t, p) != lower {
		t.Error("the confirmed save was not written")
	}
	// What the file on disk was changed to does not move the baseline: putting the baseline back
	// is not weaker than it and needs no confirmation.
	if pv, _ := s.Preview([]byte(weakFlow), hashOf([]byte(lower))); len(pv.NeedsConfirmation) != 0 {
		t.Errorf("restoring the baseline needs a confirmation: %q", pv.NeedsConfirmation)
	}
}

// A text that cannot be compared with the baseline is not "not weaker": saving it is confirmed.
func TestSaveOfATextThatCannotBeComparedNeedsConfirming(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", weakFlow)
	items := s.confirmations([]byte("nodes: ["))
	if len(items) != 1 || !strings.HasPrefix(items[0], "not compared with ") {
		t.Fatalf("%q", items)
	}
	if _, err := s.Save([]byte("nodes: ["), hashOf([]byte(weakFlow)), nil); err == nil {
		t.Error("saved without confirming")
	}
	if _, err := s.Save([]byte("nodes: ["), hashOf([]byte(weakFlow)), items); err != nil {
		t.Error(err)
	}
}

// Whatever goes wrong while writing, the file is as it was and no temporary file is left.
func TestFailedSaveLeavesTheFileAndNoDebris(t *testing.T) {
	good := osFS
	for name, fs := range map[string]fileSystem{
		"the write fails (a full disk)": {createTemp: good.createTemp, write: func(f *os.File, b []byte) error {
			_, _ = f.Write(b[:len(b)/2])
			return errors.New("no space left on device")
		}, rename: good.rename, remove: good.remove},
		"the rename fails":                  {createTemp: good.createTemp, write: good.write, rename: func(*os.Root, string, string) error { return errors.New("permission denied") }, remove: good.remove},
		"the temporary file cannot be made": {createTemp: func(*os.Root) (*os.File, string, error) { return nil, "", errors.New("read-only file system") }, write: good.write, rename: good.rename, remove: good.remove},
	} {
		s, p := newTestServer(t, "refund.yaml", weakFlow)
		s.fs = fs
		mine := strings.Replace(weakFlow, "flow: refund", "flow: other", 1)
		_, err := s.Save([]byte(mine), hashOf([]byte(weakFlow)), nil)
		if err == nil {
			t.Errorf("%s: the save reported success", name)
		}
		if readFile(t, p) != weakFlow {
			t.Errorf("%s: the file changed", name)
		}
		if names := dirFiles(t, filepath.Dir(p)); !slices.Equal(names, []string{"refund.yaml"}) {
			t.Errorf("%s: the directory holds %q", name, names)
		}
	}
}

// The path is checked again at write time. A target that became a symbolic link, to a file outside
// the directory or inside it, is not written through; nor is one that became a directory.
func TestSaveRefusesATargetThatChanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege on Windows")
	}
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mine := strings.Replace(weakFlow, "flow: refund", "flow: other", 1)
	for name, swap := range map[string]func(){
		"a symbolic link to a file outside": func() { os.Remove(p); os.Symlink(outside, p) },
		"a symbolic link inside":            func() { os.Rename(p, p+".real"); os.Symlink(p+".real", p) },
		"a directory":                       func() { os.Remove(p); os.Mkdir(p, 0o700) },
	} {
		swap()
		_, err := s.Save([]byte(mine), hashOf([]byte(weakFlow)), nil)
		if err == nil && name != "a directory" {
			// The hash check reads through the link, so a link to a file with other bytes is a
			// conflict, and one with the same bytes reaches the write check.
			t.Errorf("%s: saved", name)
		}
		if readFile(t, outside) != "outside\n" {
			t.Errorf("%s: the file outside was written", name)
		}
		os.RemoveAll(p)
		os.Remove(p + ".real")
		if err := os.WriteFile(p, []byte(weakFlow), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A link to a file with the same bytes passes the hash check and is stopped by the write check.
	same := filepath.Join(t.TempDir(), "same.yaml")
	os.WriteFile(same, []byte(weakFlow), 0o600)
	os.Remove(p)
	os.Symlink(same, p)
	if _, err := s.Save([]byte(mine), hashOf([]byte(weakFlow)), nil); err == nil || !strings.Contains(err.Error(), "escapes from parent") {
		t.Errorf("a link to a file with the same bytes: %v", err)
	}
	if readFile(t, same) != weakFlow {
		t.Error("written through the link")
	}
}

// A directory that cannot be written to is an error and the file is as it was.
func TestSaveInAReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop this user")
	}
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	if err := os.Chmod(filepath.Dir(p), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Dir(p), 0o700)
	if _, err := s.Save([]byte(weakFlow+"# x\n"), hashOf([]byte(weakFlow)), nil); err == nil {
		t.Error("saved into a directory the user cannot write to")
	}
	if readFile(t, p) != weakFlow {
		t.Error("the file changed")
	}
}

func TestSaveRefusesWhatItWillNotWrite(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	h := s.Handler()
	for name, text := range map[string]string{
		"too large": weakFlow + "# " + strings.Repeat("a", MaxFileSize) + "\n",
		"a NUL":     weakFlow + "# a\x00b\n",
	} {
		// Even with whatever confirmation the text needs, so the refusal is the size's or the NUL's own.
		_, err := s.Save([]byte(text), hashOf([]byte(weakFlow)), s.confirmations([]byte(text)))
		if err == nil || !strings.Contains(err.Error(), map[string]string{"too large": "larger than", "a NUL": "NUL"}[name]) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The same guards as every /api call, and no unknown field.
	if w := call(h, s, "/api/save", saveBody("x", "y"), func(r *http.Request) { r.Header.Del(TokenHeader) }); w.Code != 401 {
		t.Errorf("no token: %d", w.Code)
	}
	if w := call(h, s, "/api/save", `{"text":"x","hash":"y","path":"/etc/passwd"}`, nil); w.Code != 400 {
		t.Errorf("an unknown field: %d", w.Code)
	}
	if w := call(h, s, "/api/preview", `{"text":"x","hash":"y","confirmed":[]}`, nil); w.Code != 400 {
		t.Errorf("preview with an unknown field: %d", w.Code)
	}
	if readFile(t, p) != weakFlow {
		t.Error("the file changed")
	}
}

func TestPreview(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", "# header\n"+weakFlow)
	disk := "# header\n" + weakFlow
	mine := strings.Replace(strings.Replace(disk, "# header\n", "", 1), "need: 2", "need: 3", 1)
	pv, err := s.Preview([]byte(mine), hashOf([]byte(disk)))
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Changed || pv.Conflict || pv.Hash != hashOf([]byte(disk)) || !strings.Contains(pv.Diff, "-# header\n") || !strings.Contains(pv.Diff, "-      need: 2\n+      need: 3\n") {
		t.Errorf("%+v", pv)
	}
	if !slices.Equal(pv.RemovedComments, []string{"# header"}) {
		t.Errorf("removed comments %q", pv.RemovedComments)
	}
	if len(pv.NeedsConfirmation) != 0 {
		t.Errorf("a stronger text needs no confirmation: %q", pv.NeedsConfirmation)
	}
	same, _ := s.Preview([]byte(disk), hashOf([]byte(disk)))
	if same.Changed || same.Diff != "" {
		t.Errorf("%+v", same)
	}
	w := call(s.Handler(), s, "/api/preview", saveBodyPreview(mine, hashOf([]byte(disk))), nil)
	b, _ := io.ReadAll(w.Body)
	if w.Code != 200 || !strings.Contains(string(b), `"diff"`) {
		t.Errorf("%d %s", w.Code, b)
	}
}

func saveBodyPreview(text, hash string) string {
	b, _ := json.Marshal(map[string]any{"text": text, "hash": hash})
	return string(b)
}

// Two saves of one server over the same hash cannot both win: the check and the write are one
// step, so of N concurrent saves exactly one succeeds and the rest are conflicts.
func TestConcurrentSavesOverOneHashHaveOneWinner(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	hash := hashOf([]byte(weakFlow))
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	texts := make([]string, n)
	for i := range n {
		texts[i] = strings.Replace(weakFlow, "flow: refund", fmt.Sprintf("flow: w%d", i), 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Save([]byte(texts[i]), hash, nil)
		}()
	}
	wg.Wait()
	wins, winner := 0, -1
	for i, err := range errs {
		var c *Conflict
		switch {
		case err == nil:
			wins++
			winner = i
		case !errors.As(err, &c):
			t.Errorf("save %d: %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d saves succeeded, want exactly 1", wins)
	}
	if readFile(t, p) != texts[winner] {
		t.Error("the file is not the winner's text")
	}
	if names := dirFiles(t, filepath.Dir(p)); !slices.Equal(names, []string{"refund.yaml"}) {
		t.Errorf("the directory holds %q", names)
	}
}

// A file its owner made read-only is not replaced by a rename that ignores the mode.
func TestSaveDoesNotReplaceAReadOnlyFile(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	_, err := s.Save([]byte(strings.Replace(weakFlow, "flow: refund", "flow: other", 1)), hashOf([]byte(weakFlow)), nil)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("%v", err)
	}
	if readFile(t, p) != weakFlow {
		t.Error("the read-only file was replaced")
	}
}

// A file with another name is not replaced: the other name would keep the old text.
func TestSaveRefusesAFileWithHardLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard links are not counted on Windows (docs/KNOWN-LIMITATIONS.md)")
	}
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	other := filepath.Join(filepath.Dir(p), "other-name.yaml")
	if err := os.Link(p, other); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	_, err := s.Save([]byte(strings.Replace(weakFlow, "flow: refund", "flow: other", 1)), hashOf([]byte(weakFlow)), nil)
	if err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Errorf("%v", err)
	}
	if readFile(t, p) != weakFlow || readFile(t, other) != weakFlow {
		t.Error("a name was changed")
	}
	if names := dirFiles(t, filepath.Dir(p)); len(names) != 2 {
		t.Errorf("debris: %q", names)
	}
}

// A config near the size limit made of short lines and quotes is a JSON body several times its size:
// it can be checked, previewed and saved as well as opened.
func TestBodyCapAllowsAMaximalConfig(t *testing.T) {
	var b strings.Builder
	b.WriteString(weakFlow)
	for b.Len() < 900<<10 {
		b.WriteString("# \"q\"\n")
	}
	text := b.String()
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	h := s.Handler()
	body, _ := json.Marshal(map[string]string{"text": text, "hash": hashOf([]byte(weakFlow))})
	plain, _ := json.Marshal(map[string]string{"text": text})
	if len(body) < len(text)+len(text)/10 {
		t.Fatalf("the body is %d bytes for %d of text: the test is not about escaping", len(body), len(text))
	}
	if w := call(h, s, "/api/report", string(plain), nil); w.Code != 200 {
		t.Errorf("report: %d", w.Code)
	}
	if w := call(h, s, "/api/preview", string(body), nil); w.Code != 200 {
		t.Errorf("preview: %d", w.Code)
	}
	// Whole-body escapes: the same text with every line break and tab escaped is still accepted.
	tabs := strings.ReplaceAll(text, "# ", "#\t")
	tb, _ := json.Marshal(map[string]string{"text": tabs})
	if w := call(h, s, "/api/report", string(tb), nil); w.Code != 200 {
		t.Errorf("tabs: %d", w.Code)
	}
	if w := call(h, s, "/api/save", string(body), nil); w.Code != 200 && w.Code != 422 {
		t.Errorf("save: %d", w.Code)
	}
	// Over the file limit after decoding is refused, whatever the body's size.
	big, _ := json.Marshal(map[string]string{"text": strings.Repeat("# a\n", MaxFileSize/4+10)})
	if w := call(h, s, "/api/report", string(big), nil); w.Code != 413 {
		t.Errorf("a text over the limit: %d", w.Code)
	}
	_ = p
}

// A save of the text the file already holds writes nothing: the file is not touched.
func TestSaveOfTheSameTextTouchesNothing(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	if _, err := s.Save([]byte(weakFlow), hashOf([]byte(weakFlow)), nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p)
	if !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) {
		t.Errorf("a save of the same text wrote the file: %v -> %v", before.ModTime(), after.ModTime())
	}
}

// Every read and write goes through the directory handle opened at startup. A directory moved away
// and another one put at its path (with a file of the same name) is not written to: the save lands in
// the directory the editor was started on, and nothing, a temporary file included, in the new one.
func TestSaveGoesToTheDirectoryItWasStartedOn(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "flows")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "refund.yaml")
	if err := os.WriteFile(p, []byte(weakFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	moved := dir + ".moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Skipf("cannot move a directory that is open here: %v", err) // Windows
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("attacker: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Reading and saving see the original directory's file, by the handle.
	if d, err := s.Doc(); err != nil || d.Text != weakFlow {
		t.Fatalf("the read left the directory it was started on: %v", err)
	}
	mine := strings.Replace(weakFlow, "flow: refund", "flow: other", 1)
	if _, err := s.Save([]byte(mine), hashOf([]byte(weakFlow)), nil); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(moved, "refund.yaml")) != mine {
		t.Error("the save did not land in the directory the editor was started on")
	}
	if readFile(t, p) != "attacker: 1\n" || !slices.Equal(dirFiles(t, dir), []string{"refund.yaml"}) {
		t.Errorf("the directory now at the path was written to: %q", dirFiles(t, dir))
	}
}

// A temporary file a killed save left (mode 0600, the config's text, ours, old enough) is removed at
// the next start; a recent one (another editor's save in progress), another name, a directory, or a
// file that is not ours stays.
func TestStartRemovesStaleTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "refund.yaml")
	if err := os.WriteFile(p, []byte(weakFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	mk := func(name string, age time.Time) {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte(weakFlow), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, age, age); err != nil {
			t.Fatal(err)
		}
	}
	mk(tempPrefix+"deadbeef00000001", old)
	mk(tempPrefix+"deadbeef00000002", old)
	mk(tempPrefix+"recent", time.Now())
	mk("keep-me.yaml", old)
	mk(".bide-edit", old) // no dash after "edit": not ours
	if err := os.Mkdir(filepath.Join(dir, tempPrefix+"dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := dirFiles(t, dir)
	want := []string{".bide-edit", tempPrefix + "dir", tempPrefix + "recent", "keep-me.yaml", "refund.yaml"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the directory holds %q, want %q", got, want)
	}
	if readFile(t, p) != weakFlow {
		t.Error("the config changed")
	}
}

// A save leaves no temporary file under its prefix, and the one it creates is private.
func TestTemporaryFileIsPrivateAndGone(t *testing.T) {
	s, p := newTestServer(t, "refund.yaml", weakFlow)
	var mode os.FileMode
	var name string
	fs := osFS
	fs.createTemp = func(root *os.Root) (*os.File, string, error) {
		f, n, err := osFS.createTemp(root)
		if err == nil {
			if st, serr := f.Stat(); serr == nil {
				mode, name = st.Mode().Perm(), n
			}
		}
		return f, n, err
	}
	s.fs = fs
	if _, err := s.Save([]byte(weakFlow+"# x\n"), hashOf([]byte(weakFlow)), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, tempPrefix) || runtime.GOOS != "windows" && mode != 0o600 {
		t.Errorf("temporary file %q with mode %v", name, mode)
	}
	if names := dirFiles(t, filepath.Dir(p)); !slices.Equal(names, []string{"refund.yaml"}) {
		t.Errorf("debris: %q", names)
	}
}
