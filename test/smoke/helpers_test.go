package smoke

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

const testTag = "v0.1.0"

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
		if tool == "bash" && runtime.GOOS == "windows" {
			t.Skip("the smoke script tests run on Unix; the Windows script is windows.ps1")
		}
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

func (r result) all() string { return r.stdout + r.stderr }

// run executes a command in dir with env added to the process environment. A
// non-zero exit is not a test failure; failing to start is.
func run(t *testing.T, dir string, env []string, stdin string, name string, args ...string) result {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("run %s: %v", name, err)
	}
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// script runs test/smoke/<name> with bash.
func script(t *testing.T, env []string, stdin, name string, args ...string) result {
	t.Helper()
	needTools(t, "bash")
	full := append([]string{filepath.Join(repoRoot(t), "test", "smoke", name)}, args...)
	return run(t, repoRoot(t), env, stdin, "bash", full...)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// stubBin returns a directory with the given fake commands (name -> bash
// script body) and the environment that puts it first on PATH and names the
// log file the stubs append their argument lines to (STUB_LOG).
func stubBin(t *testing.T, cmds map[string]string) (env []string, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	for name, body := range cmds {
		writeFile(t, filepath.Join(dir, "bin", name), "#!/usr/bin/env bash\n"+body, 0o755)
	}
	env = []string{
		"PATH=" + filepath.Join(dir, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_LOG=" + logPath,
	}
	return env, logPath
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// minionBinary builds cmd/minion once with the version of testTag.
func minionBinary(t *testing.T) string {
	t.Helper()
	needTools(t, "go")
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "smoke-minion-")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "minion")
		cmd := exec.Command("go", "build", "-o", builtBin,
			"-ldflags", "-X github.com/patrick-1984/ai-minion/internal/version.Version="+testTag,
			"./cmd/minion")
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = errors.New("go build: " + err.Error() + "\n" + string(out))
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBin
}

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		_ = os.RemoveAll(filepath.Dir(builtBin))
	}
	os.Exit(code)
}

// fixtureAssets writes a release asset directory: the named files with the
// given contents plus checksums.txt listing all of them and the cosign
// bundles. It returns the directory.
func fixtureAssets(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	var sums strings.Builder
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names) // a fixed checksums.txt for equal input
	for _, name := range names {
		content := files[name]
		writeFile(t, filepath.Join(dir, name), content, 0o644)
		if name == "bootstrap.sh" || name == "release-manifest.json" {
			writeFile(t, filepath.Join(dir, name+".sigstore.json"), "{}", 0o644)
			continue
		}
		sums.WriteString(sha(content) + "  " + name + "\n")
	}
	writeFile(t, filepath.Join(dir, "checksums.txt"), sums.String(), 0o644)
	writeFile(t, filepath.Join(dir, "checksums.txt.sigstore.json"), "{}", 0o644)
	return dir
}

// cosignStub records its arguments and exits with $COSIGN_RC (default 0).
const cosignStub = `printf 'cosign %s\n' "$*" >>"$STUB_LOG"
exit "${COSIGN_RC:-0}"
`

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func rmFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func renameFile(from, to string) error { return os.Rename(from, to) }
