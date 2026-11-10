package smoke

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
)

// dockerBootstrap emulates "docker run" of bootstrap.sh: it logs the call and
// prints the final "ok" of the inner script.
const dockerBootstrap = `printf 'docker %s\n' "$*" >>"$STUB_LOG"
[ -z "${STUB_FAIL:-}" ] || { echo "inner script failed" >&2; exit 1; }
echo "${STUB_OUT:-ok}"
`

// AC: run bootstrap.sh in alpine and debian.
func TestBootstrapSmoke(t *testing.T) {
	needTools(t, "python3", "curl")
	env, logPath := stubBin(t, map[string]string{"docker": dockerBootstrap})
	assets := fixtureAssets(t, map[string]string{
		"minion_0.1.0_linux_amd64.tar.gz": "archive-bytes",
		"bootstrap.sh":                    "#!/bin/sh\n",
	})
	r := script(t, env, "", "bootstrap.sh", assets, testTag)
	if r.code != 0 {
		t.Fatalf("bootstrap.sh: exit %d %s", r.code, r.all())
	}
	calls := readLog(t, logPath)
	for _, image := range []string{"alpine:3.22", "debian:13"} {
		if !strings.Contains(calls, " "+image+" sh -c ") {
			t.Errorf("no run in %s:\n%s", image, calls)
		}
	}
	for _, want := range []string{
		"--network host",
		"-v " + assets + "/bootstrap.sh:/bootstrap.sh:ro",
		"-e TAG=v0.1.0",
		"-e SUM=" + sha("archive-bytes"),
		"-e MINION_RELEASE_BASE=http://127.0.0.1:",
		// The inner script covers the cosign path, the pinned checksum, a wrong
		// checksum and an offline start.
		"MINION_SHA256=$SUM",
		"checksum mismatch",
		"MINION_RELEASE_BASE=http://127.0.0.1:1",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in the docker calls:\n%s", want, calls)
		}
	}
}

func TestBootstrapSmokeFailures(t *testing.T) {
	needTools(t, "python3", "curl")
	good := map[string]string{"minion_0.1.0_linux_amd64.tar.gz": "archive-bytes", "bootstrap.sh": "#!/bin/sh\n"}
	env, _ := stubBin(t, map[string]string{"docker": dockerBootstrap})

	noBootstrap := fixtureAssets(t, map[string]string{"minion_0.1.0_linux_amd64.tar.gz": "archive-bytes"})
	if r := script(t, env, "", "bootstrap.sh", noBootstrap, testTag); r.code == 0 || !strings.Contains(r.all(), "no bootstrap.sh asset") {
		t.Errorf("missing bootstrap.sh: %d %s", r.code, r.all())
	}
	noArchive := fixtureAssets(t, map[string]string{"bootstrap.sh": "#!/bin/sh\n"})
	if r := script(t, env, "", "bootstrap.sh", noArchive, testTag); r.code == 0 || !strings.Contains(r.all(), "missing minion_0.1.0_linux_amd64.tar.gz") {
		t.Errorf("missing archive: %d %s", r.code, r.all())
	}
	tampered := fixtureAssets(t, good)
	writeFile(t, filepath.Join(tampered, "minion_0.1.0_linux_amd64.tar.gz"), "tampered", 0o644)
	if r := script(t, env, "", "bootstrap.sh", tampered, testTag); r.code == 0 || !strings.Contains(r.all(), "not in the signed checksums.txt") {
		t.Errorf("tampered archive: %d %s", r.code, r.all())
	}
	for _, e := range []string{"STUB_FAIL=1", "STUB_OUT=nope"} {
		failEnv := append(append([]string{}, env...), e)
		if r := script(t, failEnv, "", "bootstrap.sh", fixtureAssets(t, good), testTag); r.code == 0 {
			t.Errorf("%s: the smoke test passed although the container failed", e)
		}
	}
}

// tarGz returns a .tar.gz holding one executable file named name.
func tarGz(t *testing.T, name string, content []byte) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// AC (macOS): run the binary (version, worker run in the foreground with a
// private tmux socket). The script is run on this machine against the real
// binary of this checkout; only the platform names are overridden.
func TestMacosSmokeRunsTheBinaryOnAPrivateTmuxSocket(t *testing.T) {
	// The private tmux server comes from internal/testutil/tmuxtest (AD-25): it
	// decides whether tmux is available (skip, or fail with MINION_REQUIRE_TMUX=1)
	// and ends the server at cleanup. macos.sh starts and ends no server when
	// it gets the socket through SMOKE_TMUX_SOCKET.
	srv := tmuxtest.New(t)
	srv.NewSession("smoke")
	bin, err := os.ReadFile(minionBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"SMOKE_OS=linux", "SMOKE_ARCH=amd64", "TMPDIR=" + shortTemp(t), "SMOKE_TMUX_SOCKET=" + srv.Socket()}

	assets := fixtureAssets(t, map[string]string{"minion_0.1.0_linux_amd64.tar.gz": tarGz(t, "minion", bin)})
	r := script(t, env, "", "macos.sh", assets, testTag)
	if r.code != 0 {
		t.Fatalf("macos.sh: exit %d %s", r.code, r.all())
	}
	if !strings.Contains(r.stderr, "macos smoke test passed") {
		t.Errorf("unexpected output: %s", r.all())
	}

	// A binary that reports another version than the tag must fail.
	assets = fixtureAssets(t, map[string]string{"minion_0.1.1_linux_amd64.tar.gz": tarGz(t, "minion", bin)})
	r = script(t, env, "", "macos.sh", assets, "v0.1.1")
	if r.code == 0 || !strings.Contains(r.all(), "expected 'version: v0.1.1'") {
		t.Errorf("a wrong version passed: exit %d %s", r.code, r.all())
	}

	// An archive that is not in the signed checksums must fail.
	assets = fixtureAssets(t, map[string]string{"minion_0.1.0_linux_amd64.tar.gz": tarGz(t, "minion", bin)})
	writeFile(t, filepath.Join(assets, "checksums.txt"), strings.Repeat("0", 64)+"  other\n", 0o644)
	r = script(t, env, "", "macos.sh", assets, testTag)
	if r.code == 0 || !strings.Contains(r.all(), "not in the signed checksums.txt") {
		t.Errorf("an unlisted archive passed: exit %d %s", r.code, r.all())
	}
}

// shortTemp is a short temporary directory (unix socket paths are short).
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// The exit message that image.sh expects is the one the Hub prints (A-14):
// start the real Hub on a directory it cannot write to and compare, with the
// uid and the path of the image substituted.
func TestImageExpectedFixMatchesTheHubMessage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to any directory")
	}
	m := regexp.MustCompile(`(?m)^EXPECTED_FIX="([^"]+)"$`).FindStringSubmatch(readRepoFile(t, "test/smoke/image.sh"))
	if m == nil {
		t.Fatal("image.sh has no EXPECTED_FIX")
	}
	expected := m[1]

	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	r := run(t, dir, []string{"MINION_DATA_DIR=" + dir}, "", minionBinary(t), "hub", "serve")
	if r.code == 0 {
		t.Fatal("the Hub started on a read-only directory")
	}
	uid := fmt.Sprint(os.Geteuid())
	got := strings.NewReplacer(dir, "/data", "uid "+uid, "uid 65532", "chown "+uid+":", "chown 65532:").Replace(r.all())
	// The gid of the running user differs from the uid; normalize it too.
	got = regexp.MustCompile(`chown 65532:[0-9]+`).ReplaceAllString(got, "chown 65532:65532")
	if !strings.Contains(got, expected) {
		t.Errorf("the Hub says\n%s\nbut image.sh expects %q", got, expected)
	}
}

func TestHubDataDirPathInImageIsSlashData(t *testing.T) {
	// image.sh mounts the host dir at /data and sets MINION_DATA_DIR=/data, the
	// path that EXPECTED_FIX names.
	src := readRepoFile(t, "test/smoke/image.sh")
	for _, want := range []string{`-v "$1:/data"`, "MINION_DATA_DIR=/data", "data dir /data is not writable"} {
		if !strings.Contains(src, want) {
			t.Errorf("image.sh lacks %q", want)
		}
	}
}

var (
	psArgumentList = regexp.MustCompile(`-ArgumentList @\(([^)]*)\)`)
	psQuoted       = regexp.MustCompile(`'([^']*)'`)
	psEnvLiteral   = regexp.MustCompile(`(?m)^\s*\$env:(MINION_[A-Z0-9_]+)\s*=\s*'([^']*)'\s*$`)
)

// lockedBuffer is an io.Writer that can be read while a process still writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// windows.ps1 cannot run on this host, but the part that decides whether the
// smoke passes is the command line and the environment it gives the released
// binary. This test extracts the exact "worker run" argument list and the
// literal $env:MINION_* assignments from the script and runs the real binary
// with them on Linux (a private tmux server stands in for the platform's
// session host; the state directory is a throw-away). A wrong flag (the
// "--preview-only true" of an earlier version made "true" a positional
// argument and the Worker exit at once) fails here.
func TestWindowsSmokeArgvAndEnvironmentRunOnLinux(t *testing.T) {
	ps := readRepoFile(t, "test/smoke/windows.ps1")
	m := psArgumentList.FindStringSubmatch(ps)
	if m == nil {
		t.Fatal("windows.ps1 has no Start-Process -ArgumentList @(...)")
	}
	var argv []string
	for _, q := range psQuoted.FindAllStringSubmatch(m[1], -1) {
		argv = append(argv, q[1])
	}
	if len(argv) < 2 || argv[0] != "worker" || argv[1] != "run" {
		t.Fatalf("windows.ps1 starts %v, want worker run ...", argv)
	}
	var env []string
	assigned := map[string]string{}
	for _, e := range psEnvLiteral.FindAllStringSubmatch(ps, -1) {
		env = append(env, e[1]+"="+e[2])
		assigned[e[1]] = e[2]
	}
	if assigned["MINION_PREVIEW_ONLY"] != "true" {
		t.Fatalf("windows.ps1 does not set $env:MINION_PREVIEW_ONLY = 'true' (found %v)", assigned)
	}

	srv := tmuxtest.New(t)
	srv.NewSession("smoke")
	bin := minionBinary(t)
	state := filepath.Join(shortTemp(t), "state")
	runtimeDir := shortTemp(t)
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	// What the script sets itself (the state directory) and what only exists on
	// Linux (the private tmux socket, the runtime directory of the local socket).
	env = append(env, "MINION_STATE_DIR="+state, "MINION_TMUX_SOCKET="+srv.Socket(), "XDG_RUNTIME_DIR="+runtimeDir, "TMUX=", "TMUX_PANE=")
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	var logs lockedBuffer
	cmd := exec.Command(bin, argv...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(logs.String(), "worker core started") {
		select {
		case <-exited:
			t.Fatalf("the Worker exited (%v) before it started: %s", waitErr, logs.String())
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Worker did not log 'worker core started': %s", logs.String())
		}
	}
	// It must stay up (windows.ps1 waits five seconds; one is enough here).
	select {
	case <-exited:
		t.Fatalf("the Worker exited (%v) after it started: %s", waitErr, logs.String())
	case <-time.After(time.Second):
	}

	r := run(t, state, env, "", bin, "worker", "status")
	if r.code != 0 {
		t.Fatalf("worker status: exit %d %s", r.code, r.all())
	}
	for _, want := range []string{"Version: v0.1.0", "Preview only: yes"} {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(strings.SplitN(want, ": ", 2)[0]) + `:\s+` + regexp.QuoteMeta(strings.SplitN(want, ": ", 2)[1]) + `\s*$`).MatchString(r.stdout) {
			t.Errorf("worker status lacks %q:\n%s", want, r.stdout)
		}
	}
}
