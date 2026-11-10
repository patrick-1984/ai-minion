package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAcceptsAGoodTree(t *testing.T) {
	root := t.TempDir()
	write(t, root, "README.md", "# R\n[g](docs/guides/a.md#the-heading) [s](docs/security.md)\n")
	write(t, root, "docs/guides/a.md", "Back to the [README](../../README.md).\n## The `heading`\n")
	write(t, root, "docs/security.md", "Back to the [README](../README.md).\n")
	if p, err := check(root); err != nil || len(p) != 0 {
		t.Fatalf("problems %v, err %v", p, err)
	}
}

func TestCheckReportsEachKindOfProblem(t *testing.T) {
	root := t.TempDir()
	write(t, root, "README.md", "# R\n[g](docs/guides/a.md#nope) [m](docs/missing.md) [x](https://example.com/x)\n"+
		"```\n[in code](docs/ignored.md)\n```\n")
	write(t, root, "docs/guides/a.md", "no backlink here\n## Heading\n")
	write(t, root, "docs/guides/b.md", "Back to the [README](../../README.md).\n")
	p, err := check(root)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(p, "\n")
	for _, want := range []string{
		"has no heading #nope", "docs/missing.md does not exist",
		"docs/guides/a.md: no link back to README.md",
		"README.md: no link to docs/guides/b.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing problem %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ignored.md") || strings.Contains(got, "example.com") {
		t.Errorf("a fenced or external link was checked:\n%s", got)
	}
}

func TestSlugMatchesGitHub(t *testing.T) {
	for in, want := range map[string]string{
		"Reaching the web UI the same way": "reaching-the-web-ui-the-same-way",
		"The `--force` flag, (2)":          "the---force-flag-2",
		"[Link](x.md) text":                "link-text",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}
