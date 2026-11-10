// Command checkdocs checks the user documentation (README.md, CONTRIBUTING.md,
// docs/install, docs/guides and docs/security.md; Story 9.8): every relative
// Markdown link points at a file or directory that exists, every "#fragment"
// names a heading of the target file, every page of docs/install, docs/guides
// and docs/security.md links back to the README, and the README links to each
// of those pages. Links to other sites are not fetched (the check needs no
// network). Fenced code blocks are not searched for links.
//
// Usage: go run ./scripts/checkdocs [repo-root]   (default: the current directory)
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// [text](target) and [text](target "title"); an image has a "!" before.
	linkRe    = regexp.MustCompile(`\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	headingRe = regexp.MustCompile(`^ {0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	fenceRe   = regexp.MustCompile("^ {0,3}(```|~~~)")
	slugDrop  = regexp.MustCompile(`[^\p{L}\p{N}\s_-]`)
	codeSpan  = regexp.MustCompile("`[^`]*`")
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	problems, err := check(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "checkdocs:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "checkdocs:", p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
	fmt.Println("checkdocs: ok")
}

// check returns the problems found under root (slash-separated paths).
func check(root string) ([]string, error) {
	var files []string
	for _, f := range []string{"README.md", "CONTRIBUTING.md"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			files = append(files, f)
		}
	}
	// The user documentation only: docs/plan is generated from EPICS.md and
	// docs/design and docs/architecture are working documents.
	var err error
	for _, dir := range []string{"docs/install", "docs/guides"} {
		err = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if !d.IsDir() && strings.HasSuffix(p, ".md") {
				rel, _ := filepath.Rel(root, p)
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docs/security.md")); err == nil {
		files = append(files, "docs/security.md")
	}
	sort.Strings(files)

	var problems []string
	links := map[string][]string{} // file -> resolved repo-relative link targets (no fragment)
	slugs := map[string]map[string]bool{}
	slugsOf := func(rel string) map[string]bool {
		if s, ok := slugs[rel]; ok {
			return s
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			slugs[rel] = nil
			return nil
		}
		slugs[rel] = headingSlugs(string(data))
		return slugs[rel]
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			return nil, err
		}
		for _, l := range extractLinks(string(data)) {
			target := l.target
			if isExternal(target) {
				continue
			}
			file, frag, _ := strings.Cut(target, "#")
			resolved := f
			if file != "" {
				resolved = path.Clean(path.Join(path.Dir(f), file))
			}
			if strings.HasPrefix(resolved, "../") || resolved == ".." {
				problems = append(problems, fmt.Sprintf("%s:%d: link %q leaves the repository", f, l.line, target))
				continue
			}
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(resolved)))
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: link %q: %s does not exist", f, l.line, target, resolved))
				continue
			}
			links[f] = append(links[f], resolved)
			if frag != "" && !info.IsDir() && strings.HasSuffix(resolved, ".md") {
				if s := slugsOf(resolved); !s[strings.ToLower(frag)] {
					problems = append(problems, fmt.Sprintf("%s:%d: link %q: %s has no heading #%s", f, l.line, target, resolved, frag))
				}
			}
		}
	}

	// Backlinks and the README index of the user documentation.
	var pages []string
	for _, f := range files {
		if strings.HasPrefix(f, "docs/install/") || strings.HasPrefix(f, "docs/guides/") || f == "docs/security.md" {
			pages = append(pages, f)
		}
	}
	for _, p := range pages {
		if !contains(links[p], "README.md") {
			problems = append(problems, fmt.Sprintf("%s: no link back to README.md", p))
		}
		if !contains(links["README.md"], p) {
			problems = append(problems, fmt.Sprintf("README.md: no link to %s", p))
		}
	}
	return problems, nil
}

type link struct {
	target string
	line   int
}

// extractLinks returns the inline links outside fenced code blocks and code
// spans.
func extractLinks(text string) []link {
	var out []link
	fence := ""
	for i, line := range strings.Split(text, "\n") {
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			switch fence {
			case "":
				fence = m[1]
			case m[1]:
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		line = codeSpan.ReplaceAllString(line, "")
		for _, m := range linkRe.FindAllStringSubmatch(line, -1) {
			out = append(out, link{target: m[1], line: i + 1})
		}
	}
	return out
}

// headingSlugs returns the GitHub anchors of the headings in text, including
// the "-1", "-2" suffixes of repeated headings.
func headingSlugs(text string) map[string]bool {
	slugs := map[string]bool{}
	seen := map[string]int{}
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			switch fence {
			case "":
				fence = m[1]
			case m[1]:
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		m := headingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		s := slug(m[1])
		if n := seen[s]; n > 0 {
			slugs[fmt.Sprintf("%s-%d", s, n)] = true
		} else {
			slugs[s] = true
		}
		seen[s]++
	}
	return slugs
}

// slug is the GitHub heading anchor: lower case, links and code marks removed,
// punctuation dropped, spaces turned into hyphens.
func slug(h string) string {
	h = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`).ReplaceAllString(h, "$1")
	h = strings.ReplaceAll(h, "`", "")
	h = strings.ToLower(strings.TrimSpace(h))
	h = slugDrop.ReplaceAllString(h, "")
	return strings.ReplaceAll(h, " ", "-")
}

func isExternal(t string) bool {
	if i := strings.Index(t, ":"); i > 0 && !strings.ContainsAny(t[:i], "/#?") {
		return true // http:, https:, mailto:, ...
	}
	return strings.HasPrefix(t, "//")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
