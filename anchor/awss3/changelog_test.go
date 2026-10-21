package awss3_test

import (
	"os"
	"path/filepath"
	"testing"
)

// changelogEntry returns the text a changelog check reads: the unreleased fragment while it exists, and
// CHANGELOG.md once a release has assembled the fragments into it. A check of what an entry says must
// keep running after the release (the integration job fails on a skipped test), so it never skips.
func changelogEntry(t *testing.T, fragment string) string {
	t.Helper()
	root := filepath.Join("..", "..")
	b, err := os.ReadFile(filepath.Join(root, "changes", "unreleased", fragment))
	if err == nil {
		return string(b)
	}
	if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	b, err = os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
