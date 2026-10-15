package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Slack and PagerDuty through the relay": "slack-and-pagerduty-through-the-relay",
		"`bide approve`":                        "bide-approve",
		"What an approver signs":                "what-an-approver-signs",
		"A [link](x.md) here, 1-of-1":           "a-link-here-1-of-1",
		"Policy assertions: the `assert` list":  "policy-assertions-the-assert-list",
		"m-of-n_approval":                       "m-of-n_approval",
		"Überblick (v2)":                        "überblick-v2",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeadingAnchors(t *testing.T) {
	got := headingAnchors("# Top\n## Same\n## Same\n## Same\n```\n# not a heading\n```\n<a id=\"explicit\"></a>\n#### Closed ####\n")
	for _, want := range []string{"top", "same", "same-1", "same-2", "explicit", "closed"} {
		if !got[want] {
			t.Errorf("missing anchor %q in %v", want, got)
		}
	}
	if got["not-a-heading"] {
		t.Error("a heading inside a fenced block is an anchor")
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCheckLinks(t *testing.T) {
	root := writeTree(t, map[string]string{
		"README.md": "[ok](docs/a.md#second-part) [dir](docs/guides) [img](assets/x.png)\n" +
			"[missing file](docs/nope.md)\n" +
			"[bad anchor](docs/a.md#nope)\n" +
			"[abs](https://example.com/x.md#y) [mail](mailto:a@b.c) [site](/guides/a)\n" +
			"`[code](nope.md)`\n```\n[fenced](nope.md)\n```\n",
		"assets/x.png":        "x",
		"docs/guides/g.md":    "# G\nsee [up](../a.md#second-part), [self](#g), [self bad](#h), [stub](stub.md#a)\n",
		"docs/guides/stub.md": "moved\n",
		"docs/a.md":           "# A\n## Second part\n<img src=\"missing.png\">\n",
		"docs/releases/r.md":  "[not checked](nope.md)\n",
		"docs/IMPL/plan.md":   "[not checked](nope.md)\n",
	})
	got, err := CheckLinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"README.md:2: broken link docs/nope.md: no such file or directory",
		"README.md:3: broken link docs/a.md#nope: no heading or anchor #nope in a.md",
		"README.md:4: broken link /guides/a: an absolute path",
		"docs/a.md:3: broken link missing.png: no such file or directory",
		"docs/guides/g.md:2: broken link #h: no heading or anchor #h in g.md",
		"docs/guides/g.md:2: broken link stub.md#a: no heading or anchor #a in stub.md",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("finding %d = %q, want prefix %q", i, got[i], want[i])
		}
	}
}

func TestCheckLinksClean(t *testing.T) {
	root := writeTree(t, map[string]string{"README.md": "[a](docs/a.md#a)\n", "docs/a.md": "# A\n"})
	got, err := CheckLinks(root)
	if err != nil || len(got) != 0 {
		t.Fatalf("CheckLinks = %v, %v; want none", got, err)
	}
}

func TestRunLinks(t *testing.T) {
	root := writeTree(t, map[string]string{"README.md": "[gone](docs/gone.md)\n", "docs/a.md": "# A\n"})
	var stdout, stderr strings.Builder
	if code := run([]string{"-root", root, "-links"}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "README.md:1: broken link docs/gone.md") {
		t.Fatalf("-links on a broken tree: code %d, stdout %q", code, stdout.String())
	}
	// The default run reports the same broken link beside the other checks.
	stdout.Reset()
	if code := run([]string{"-root", root, "-allow", ""}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "broken link docs/gone.md") {
		t.Fatalf("default run on a broken tree: code %d, stdout %q", code, stdout.String())
	}
	clean := writeTree(t, map[string]string{"README.md": "[a](docs/a.md#a)\n", "docs/a.md": "# A\n"})
	if code := run([]string{"-root", clean, "-links"}, &stdout, &stderr); code != 0 {
		t.Fatalf("-links on a clean tree: code %d", code)
	}
}
