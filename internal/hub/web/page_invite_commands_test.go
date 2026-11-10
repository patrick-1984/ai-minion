package web

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The generated "key in a file" commands, executed by a real POSIX shell with a
// fake minion on the PATH that records what it was given. The shell script is
// what a user pastes into a terminal, so it is tested as one.

const fakeMinion = `#!/bin/sh
# Records what the Worker command saw, then exits with $FAKE_MINION_STATUS.
out="$FAKE_MINION_OUT"
printf '%s\n' "$@" > "$out/argv"
file=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--invite-key" ]; then file="$a"; fi
  prev="$a"
done
printf '%s' "$file" > "$out/file"
cat "$file" > "$out/content" 2>/dev/null
ls -ld "$file" | cut -c1-10 | tr -d "\n" > "$out/filemode"
ls -ld "$(dirname "$file")" | cut -c1-10 | tr -d "\n" > "$out/dirmode"
exit "${FAKE_MINION_STATUS:-0}"
`

type fileVariantRun struct {
	cwd, out string
	err      error
	output   string
}

// runFileVariant runs the file variant for sampleKey in cwd. tmpdir is TMPDIR
// for the run; umask is the umask of the calling shell.
func runFileVariant(t *testing.T, cwd, tmpdir, umask string, status string) fileVariantRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the file variant is a POSIX shell script")
	}
	bin, out := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "minion"), []byte(fakeMinion), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "umask " + umask + "\n" + fileVariant("https://hub.example.com", sampleKey)
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = cwd
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "TMPDIR=" + tmpdir, "FAKE_MINION_OUT=" + out, "FAKE_MINION_STATUS=" + status}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return fileVariantRun{cwd: cwd, out: out, err: err, output: buf.String()}
}

func (r fileVariantRun) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.out, name))
	if err != nil {
		t.Fatalf("the Worker command did not run (%s): %v\n%s", name, err, r.output)
	}
	return string(b)
}

func TestFileVariantCreatesAPrivateFileAndRemovesIt(t *testing.T) {
	cwd, tmp := t.TempDir(), t.TempDir()
	// A permissive calling shell: the script itself must make the file private.
	r := runFileVariant(t, cwd, tmp, "022", "0")
	if r.err != nil {
		t.Fatalf("run: %v\n%s", r.err, r.output)
	}
	file := r.read(t, "file")
	if filepath.Dir(file) == cwd || !strings.HasPrefix(file, tmp+"/minion-invite.") || filepath.Base(file) != "invite.key" {
		t.Errorf("the key file is %q, want invite.key in a fresh minion-invite.* directory under TMPDIR", file)
	}
	if got := r.read(t, "content"); got != sampleKey+"\n" {
		t.Errorf("the Worker command read %q", got)
	}
	if got := r.read(t, "filemode"); got != "-rw-------" {
		t.Errorf("file mode = %s, want -rw-------", got)
	}
	if got := r.read(t, "dirmode"); got != "drwx------" {
		t.Errorf("directory mode = %s, want drwx------", got)
	}
	// The key is not an argument, and nothing is left behind.
	if argv := r.read(t, "argv"); strings.Contains(argv, "mnk_") || !strings.Contains(argv, "--server\nhttps://hub.example.com\n--invite-key\n") {
		t.Errorf("argv = %q", argv)
	}
	for _, dir := range []string{cwd, tmp} {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s is not empty after the run: %v", dir, entries)
		}
	}
}

// The old variant wrote ./invite.key with a redirection: an existing 0644 file
// kept its mode and the key became world-readable, and a symbolic link sent the
// key to wherever it pointed. The new one never touches either.
func TestFileVariantIgnoresAnExistingPermissiveFileAndASymlink(t *testing.T) {
	cwd, tmp, victimDir := t.TempDir(), t.TempDir(), t.TempDir()

	old := filepath.Join(cwd, "invite.key")
	if err := os.WriteFile(old, []byte("old content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(victimDir, "victim")
	if err := os.WriteFile(victim, []byte("do not touch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A planted link where a predictable name in TMPDIR would be, too.
	for _, name := range []string{"invite.key", "minion-invite.key", "minion-invite.XXXXXX"} {
		if err := os.Symlink(victim, filepath.Join(tmp, name)); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(cwd, "link", "invite.key")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{cwd, filepath.Dir(link)} {
		r := runFileVariant(t, dir, tmp, "022", "0")
		if r.err != nil {
			t.Fatalf("run in %s: %v\n%s", dir, r.err, r.output)
		}
		if got := r.read(t, "filemode"); got != "-rw-------" {
			t.Errorf("file mode = %s, want -rw-------", got)
		}
		if got := r.read(t, "content"); got != sampleKey+"\n" {
			t.Errorf("content = %q", got)
		}
	}
	for path, want := range map[string]string{old: "old content\n", victim: "do not touch\n"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want it untouched (%q)", path, got, err, want)
		}
	}
	if fi, err := os.Stat(victim); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("victim = %v, %v", fi, err)
	}
}

// When the private file cannot be created the Worker command does not run, and
// when it runs and fails its status is the script's status.
func TestFileVariantStopsWithoutTheFileAndKeepsTheWorkerStatus(t *testing.T) {
	cwd := t.TempDir()
	r := runFileVariant(t, cwd, filepath.Join(cwd, "no-such-dir"), "022", "0")
	if r.err == nil {
		t.Errorf("the script succeeded without a private directory:\n%s", r.output)
	}
	if _, err := os.Stat(filepath.Join(r.out, "argv")); err == nil {
		t.Errorf("the Worker command ran without its key file:\n%s", r.output)
	}
	if strings.Contains(r.output, "mnk_") {
		t.Errorf("the output holds the key:\n%s", r.output)
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Errorf("files left behind: %v", entries)
	}

	tmp := t.TempDir()
	r = runFileVariant(t, cwd, tmp, "022", "7")
	if ee, ok := r.err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Errorf("err = %v, want exit status 7 of the Worker command", r.err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("the key file was not removed after a failed run: %v", entries)
	}
}

// The script runs in a subshell: it leaves the umask of the user's shell alone.
func TestFileVariantLeavesTheCallersShellAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	bin, out, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "minion"), []byte(fakeMinion), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "umask 022\n"+fileVariant("https://hub.example.com", sampleKey)+"umask; trap")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "TMPDIR=" + tmp, "FAKE_MINION_OUT=" + out}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got := strings.TrimSpace(string(b)); got != "0022" && got != "022" {
		t.Errorf("umask after the script = %q, want the caller's 022 and no trap output", got)
	}
}

// -- the Compose project -----------------------------------------------------

// composeLine returns the trimmed text after prefix on the first line of s that
// starts with it, after trimming.
func composeLine(t *testing.T, s, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(l, prefix))
		}
	}
	t.Fatalf("no line starting with %q in:\n%s", prefix, s)
	return ""
}

// The Worker project must start a Worker: bootstrap.sh is the entrypoint and
// ends in `exec minion "$@"`, so the command holds the arguments of minion and
// not its name; tmux is installed; the key is a secret file, never in the text.
func TestComposeProjectStartsAWorker(t *testing.T) {
	c := composeFile("https://hub.example.com")
	if strings.Contains(c, sampleKey) || strings.Contains(c, "mnk_") {
		t.Errorf("the project holds a key:\n%s", c)
	}
	script, err := os.ReadFile("../../../deploy/bootstrap.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), `exec minion "$@"`) {
		t.Fatal("deploy/bootstrap.sh no longer ends in exec minion \"$@\"; the Compose command must follow it")
	}
	if got := composeLine(t, c, "entrypoint:"); got != `["/bin/sh", "/bootstrap.sh"]` {
		t.Errorf("entrypoint = %s", got)
	}
	if !strings.Contains(c, "\n    command: [\"worker\", \"up\"]\n") {
		t.Errorf("the Worker service does not run [\"worker\", \"up\"]:\n%s", c)
	}
	if strings.Contains(c, `"minion", "worker"`) || strings.Contains(c, `["minion"`) {
		t.Errorf("the command repeats the binary name that bootstrap.sh already runs:\n%s", c)
	}
	for _, want := range []string{
		"apk add --no-cache tmux", // the Worker drives its sessions through tmux
		"./bootstrap.sh:/bootstrap.sh:ro",
		"MINION_INVITE_KEY_FILE: /run/secrets/minion_invite_key",
		"file: ./invite_key.txt",
		`MINION_SERVER: "https://hub.example.com"`,
		"MINION_STATE_DIR: /data",
		"MINION_CACHE_DIR: /cache",
		"service_completed_successfully",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("the project lacks %q:\n%s", want, c)
		}
	}
	// The mounted key file must stay: Docker needs the source of a bind mount to
	// exist when the container restarts. The text says to keep it, erases it in
	// place through the root init service (the file belongs to the Worker's user,
	// mode 600) and never tells the user to delete it.
	for _, want := range []string{
		"KEEP the file invite_key.txt",
		"docker compose run --rm --no-deps init sh -c ': > /invite_key.txt'",
		"remove invite_key.txt from init's volumes, the secret and",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("the project lacks the instruction %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "you can delete invite_key.txt") || strings.Contains(c, "not needed and you can delete") {
		t.Errorf("the project tells the user to delete the mounted key file:\n%s", c)
	}
	// Not the Hub image (no tmux, no shell), and not the Hub's user.
	if strings.Contains(c, "ghcr.io/") || strings.Contains(c, "65532") || strings.Contains(c, "YOUR-IMAGE") {
		t.Errorf("the project depends on an image the user does not have:\n%s", c)
	}
	// Compose interpolates $ in values: a $ in the address is escaped.
	if got := composeFile("https://hub.example.com/a$b"); !strings.Contains(got, `"https://hub.example.com/a$$b"`) {
		t.Errorf("a $ in the address is not escaped:\n%s", got)
	}
}

// With a Docker CLI at hand, `docker compose config` (which needs no daemon)
// accepts the project and resolves it as intended.
func TestComposeProjectIsValidCompose(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("no docker CLI")
	}
	if err := exec.Command(docker, "compose", "version").Run(); err != nil {
		t.Skip("no docker compose plugin")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeFile("https://hub.example.com")), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(docker, "compose", "-f", filepath.Join(dir, "compose.yaml"), "config", "--format", "json")
	cmd.Env = append(os.Environ(), "MINION_VERSION=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, out)
	}
	var cfg struct {
		Services map[string]struct {
			Entrypoint  []string          `json:"entrypoint"`
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
			User        string            `json:"user"`
			Build       struct {
				DockerfileInline string `json:"dockerfile_inline"`
			} `json:"build"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	w, ok := cfg.Services["minion-worker"]
	if !ok {
		t.Fatalf("no minion-worker service:\n%s", out)
	}
	if strings.Join(append(w.Entrypoint, w.Command...), " ") != "/bin/sh /bootstrap.sh worker up" {
		t.Errorf("the container runs %v %v", w.Entrypoint, w.Command)
	}
	if !strings.Contains(w.Build.DockerfileInline, "tmux") || w.User != "10001:10001" {
		t.Errorf("worker = %+v", w)
	}
	if w.Environment["MINION_SERVER"] != "https://hub.example.com" || w.Environment["MINION_VERSION"] != "latest" {
		t.Errorf("environment = %v", w.Environment)
	}
}
