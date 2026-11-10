package releasecheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is the repository root (this package lives in scripts/releasecheck).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
}

// result is the outcome of a script run.
type result struct {
	code   int
	stdout string
	stderr string
}

func (r result) combined() string { return r.stdout + r.stderr }

// run executes a command and returns its exit code and output. A non-zero
// exit is not a test failure; failing to start the command is.
func run(t *testing.T, dir string, env []string, name string, args ...string) result {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := result{stdout: out.String(), stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.code = ee.ExitCode()
	default:
		t.Fatalf("run %s: %v", name, err)
	}
	return res
}

// section returns the top-level YAML block that starts with "<key>:" (up to
// the next top-level key). The configuration is checked as text on purpose:
// the project has no YAML library and the checks need only substrings.
func section(t *testing.T, doc, key string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `:.*$`)
	loc := re.FindStringIndex(doc)
	if loc == nil {
		t.Fatalf("top-level key %q not found", key)
	}
	rest := doc[loc[0]:]
	next := regexp.MustCompile(`(?m)\n[A-Za-z#]`).FindStringIndex(rest[1:])
	if next == nil {
		return rest
	}
	return rest[:next[0]+1]
}

func mustContain(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s lacks %q", what, w)
		}
	}
}

func mustNotContain(t *testing.T, what, text string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(text, b) {
			t.Errorf("%s must not contain %q", what, b)
		}
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func lookPath(name string) (string, error) { return exec.LookPath(name) }
