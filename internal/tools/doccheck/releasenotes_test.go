package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelativeLinks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"absolute image", "![a](https://raw.githubusercontent.com/bide-ai/bide/v0.15.0/x.png)", nil},
		{"relative image", "![a](../guides/img/x.png)", []string{"../guides/img/x.png"}},
		{"relative link", "see [the guide](docs/guides/a.md)", []string{"docs/guides/a.md"}},
		{"anchor and mailto", "[top](#top) [mail](mailto:a@b.c)", nil},
		{"html img", `<img src="assets/x.png">`, []string{"assets/x.png"}},
		{"html img absolute", `<img src="https://example.com/x.png">`, nil},
		{"inline code", "`ResumeTyped[T](a)` and `x[0](y)`", nil},
		{"fenced block", "```go\nf := g[T](a)\n```\n", nil},
		{"code then link", "`a[b](c)` then [d](e.md)", []string{"e.md"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := relativeLinks(c.in)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("relativeLinks(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestCheckReleaseNotes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "releases")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("v1.0.0.md", "![ok](https://example.com/a.png)\n")
	write("v1.1.0.md", "line one\n![bad](../guides/img/a.png)\n")
	write("notes.txt", "![ignored](rel.png)\n")
	got, err := CheckReleaseNotes(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "v1.1.0.md:2") || !strings.Contains(got[0], "../guides/img/a.png") {
		t.Fatalf("CheckReleaseNotes = %q, want one finding for v1.1.0.md:2", got)
	}
}

// TestRepoReleaseNotes keeps the repository's own release notes free of relative links: each file
// becomes a GitHub Release body, where a relative path does not resolve.
func TestRepoReleaseNotes(t *testing.T) {
	got, err := CheckReleaseNotes(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Error(f)
	}
}
