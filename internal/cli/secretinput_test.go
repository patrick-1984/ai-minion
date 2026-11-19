package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// endlessPipe returns its data, then blocks until closed: a pipe that
// never ends.
type endlessPipe struct {
	mu     sync.Mutex
	data   []byte
	closed chan struct{}
	reads  int
}

func newEndlessPipe(data string) *endlessPipe {
	return &endlessPipe{data: []byte(data), closed: make(chan struct{})}
}

func (r *endlessPipe) Read(p []byte) (int, error) {
	r.mu.Lock()
	r.reads++
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		r.mu.Unlock()
		return n, nil
	}
	r.mu.Unlock()
	<-r.closed
	return 0, io.EOF
}

func TestStdinReaderCancel(t *testing.T) {
	text, _ := testJoinToken(t, "https://hub.example:8443")
	in := newEndlessPipe(text)
	defer close(in.closed)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	raw, err := readSecretInput(ctx, "-", in, maxSecretInput, "the join token")
	if !errors.Is(err, errSecretInterrupted) || !errors.Is(err, context.Canceled) || raw != nil {
		t.Fatalf("read = %d bytes, %v; want interrupted", len(raw), err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the cancel took %s", d)
	}
	if strings.Contains(err.Error(), text) {
		t.Fatal("the error holds the token")
	}

	// A context that already ended reads nothing.
	done, stop := context.WithCancel(context.Background())
	stop()
	unread := newEndlessPipe(text)
	defer close(unread.closed)
	if _, err := readSecretInput(done, "-", unread, maxSecretInput, "the join token"); !errors.Is(err, errSecretInterrupted) || unread.reads != 0 {
		t.Fatalf("ended context: %v, %d reads", err, unread.reads)
	}

	// The join flow maps it to exit 130, nothing changed.
	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel2)
	in2 := newEndlessPipe(text)
	defer close(in2.closed)
	_, err = readJoinToken(ctx2, "-", in2)
	var re *resultError
	if !errors.As(err, &re) || re.Exit != ExitInterrupted || strings.Contains(err.Error(), text) {
		t.Fatalf("join read = %v", err)
	}
}

// countingReader is an endless stream of 'a' that counts what was read.
type countingReader struct{ n int }

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	r.n += len(p)
	return len(p), nil
}

func TestStdinReaderBound(t *testing.T) {
	ctx := context.Background()
	exact := strings.Repeat("a", maxSecretInput)
	raw, err := readSecretInput(ctx, "-", strings.NewReader(exact), maxSecretInput, "the join token")
	if err != nil || len(raw) != maxSecretInput {
		t.Fatalf("exactly the bound: %d bytes, %v", len(raw), err)
	}
	raw, err = readSecretInput(ctx, "-", strings.NewReader(exact+"a"), maxSecretInput, "the join token")
	if !errors.Is(err, errSecretTooLong) || raw != nil {
		t.Fatalf("one byte over: %d bytes, %v", len(raw), err)
	}
	// An endless stream ends at the bound: it is not read to its end.
	endless := &countingReader{}
	if _, err := readSecretInput(ctx, "-", endless, maxSecretInput, "the join token"); !errors.Is(err, errSecretTooLong) {
		t.Fatalf("endless: %v", err)
	}
	if endless.n > 4*maxSecretInput {
		t.Fatalf("read %d bytes of an endless stream, bound %d", endless.n, maxSecretInput)
	}
	// A file over the bound too, and the join flow calls it malformed
	// without repeating the token.
	text, _ := testJoinToken(t, "https://hub.example:8443")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(text+strings.Repeat(" ", maxSecretInput)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = readJoinToken(ctx, path, nil)
	if err == nil || !strings.Contains(err.Error(), "too long") || strings.Contains(err.Error(), text) || strings.Contains(err.Error(), path) {
		t.Fatalf("long file: %v", err)
	}
	if _, err := readJoinToken(ctx, "-", nil); err == nil {
		t.Fatal("no stdin accepted")
	}
}

// The token's Hub fingerprint differs from the configured hub_fingerprint
// (config file, environment or flag): the join stops before the invitation
// key reaches any Hub, and nothing is saved.
func TestMismatchBeforeInviteSent(t *testing.T) {
	other, err := identity.FingerprintOf(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := testJoinToken(t, "https://hub.example:8443")
	for _, where := range []string{"file", "env", "flag"} {
		t.Run(where, func(t *testing.T) {
			c := newJoinCLI(t)
			args := []string{"--token-file", "-", "--share-existing", "all"}
			switch where {
			case "file":
				writeConfigFile(t, c.state, map[string]string{config.KeyHubFingerprint: string(other)})
			case "env":
				c.env["MINION_HUB_FINGERPRINT"] = string(other)
			case "flag":
				args = append(args, "--hub-fingerprint", string(other))
			}
			m, code := c.run(text, args...)
			if code != ExitError || m["ok"] != false {
				t.Fatalf("join = %v, %d", m, code)
			}
			c.invite.mu.Lock()
			keys := len(c.invite.keys)
			c.invite.mu.Unlock()
			if keys != 0 || c.invite.n() != 0 {
				t.Fatalf("the Hub got %d invitation keys", keys)
			}
			if _, ok := m["enrollment"]; ok {
				t.Fatalf("an enrollment stage was reported: %v", m)
			}
			if got := c.config(); got[config.KeyServer] != "" {
				t.Fatalf("config saved: %v", got)
			}
			if _, err := os.Stat(filepath.Join(c.state, "worker.db")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the Worker state was written before the check: %v", err)
			}
		})
	}
}

// joinLeakChildEnv selects the child mode of TestJoinSecretNeverLeaks.
const joinLeakChildEnv = "MINION_TEST_JOIN_LEAK_CHILD"

// joinLeakGateEnv makes the child wait for the end of file descriptor 3
// before it runs (Linux: the parent reads its command line meanwhile).
const joinLeakGateEnv = "MINION_TEST_JOIN_LEAK_GATE"

// joinLeakReadMark is written to stderr by the child when the join first
// reads stdin, so the parent knows when to interrupt it.
const joinLeakReadMark = "join-leak-child: reading stdin"

// markedStdin writes joinLeakReadMark on its first read.
type markedStdin struct {
	once sync.Once
	in   io.Reader
}

func (m *markedStdin) Read(p []byte) (int, error) {
	m.once.Do(func() { _, _ = fmt.Fprintln(os.Stderr, joinLeakReadMark) })
	return m.in.Read(p)
}

// runJoinLeakChild is the child: "minion worker join <args>" through the
// real command, with the real stdin, stdout and stderr of the process, a
// fake Hub dialer and a fake running Worker.
func runJoinLeakChild() {
	if os.Getenv(joinLeakGateEnv) == "1" {
		// Wait until the parent has read this process's command line.
		gate := os.NewFile(3, "gate")
		_, _ = io.Copy(io.Discard, gate)
		_ = gate.Close()
	}
	state := os.Getenv("MINION_STATE_DIR")
	dialer := &fakeInviteDialer{}
	worker := &fakeLiveWorker{state: state, running: true, instance: 1}
	deps := workerPairDeps{
		stdin:        &markedStdin{in: os.Stdin},
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		lookupEnv:    os.LookupEnv,
		loadConfig:   loadWorkerConfig,
		saveServer:   saveWorkerHub,
		inviteDialer: dialer,
		call:         worker.call,
		restartSleep: func(time.Duration) {},
		restartWait:  2 * time.Second,
	}
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerJoinCmd(deps))
	code := execute(context.Background(), root, flag.Args(), os.Stderr, os.Stderr)
	_, _ = fmt.Fprintf(os.Stderr, "join-leak-child: invites=%d\n", dialer.n())
	os.Exit(code)
}

// lockedBuffer is a bytes.Buffer safe for the exec copy goroutine; it
// signals mark once its text appears.
type lockedBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	mark string
	seen chan struct{}
	once sync.Once
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if b.mark != "" && strings.Contains(b.buf.String(), b.mark) {
		b.once.Do(func() { close(b.seen) })
	}
	return n, err
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestJoinSecretNeverLeaks runs "minion worker join --token-file" in a child
// process for success, malformed input, a fingerprint mismatch, an
// interrupt, a missing and an unreadable file. Neither the token nor its
// inner invitation key (text form, secret part or raw bytes) may appear in
// the child's command line, its stdout JSON, its stderr (where its logger
// writes) or any file under its state, temp and home directories.
func TestJoinSecretNeverLeaks(t *testing.T) {
	if os.Getenv(joinLeakChildEnv) == "1" {
		runJoinLeakChild()
		return
	}
	text, tok := testJoinToken(t, "https://hub.example:8443")
	key := tok.Invite.String()
	secretPart := key[strings.LastIndex(key, ".")+1:]
	secrets := map[string][]byte{
		"token": []byte(text), "invitation key": []byte(key),
		"key secret": []byte(secretPart), "raw secret": tok.Invite.Secret,
	}
	other, err := identity.FingerprintOf(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	malformed := text[:len(text)-8] + "!!!!!!!!"
	inputs := t.TempDir() // input files are not scanned: the token is there on purpose
	tokenFile := filepath.Join(inputs, "token")
	if err := os.WriteFile(tokenFile, []byte(text+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(inputs, "unreadable")
	if err := os.WriteFile(unreadable, []byte(text+"\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(inputs, "missing")

	type leakCase struct {
		name    string
		stdin   string
		args    []string
		env     []string
		code    int
		invites int
		cancel  bool
	}
	cases := []leakCase{
		{name: "success", stdin: text + "\n", args: []string{"--token-file", "-"}, code: ExitOK, invites: 1},
		{name: "success-file", args: []string{"--token-file", tokenFile}, code: ExitOK, invites: 1},
		{name: "malformed", stdin: malformed + "\n", args: []string{"--token-file", "-"}, code: ExitError},
		{name: "mismatch", stdin: text + "\n", args: []string{"--token-file", "-"}, env: []string{"MINION_HUB_FINGERPRINT=" + string(other)}, code: ExitError},
		{name: "cancel", stdin: text, args: []string{"--token-file", "-"}, code: ExitInterrupted, cancel: true},
		{name: "missing-file", args: []string{"--token-file", missing}, code: ExitError},
		{name: "unreadable-file", args: []string{"--token-file", unreadable}, code: ExitError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cancel && runtime.GOOS == "windows" {
				t.Skip("an interrupt cannot be sent to a child process on Windows")
			}
			if tc.name == "unreadable-file" {
				if runtime.GOOS == "windows" || os.Geteuid() == 0 {
					t.Skip("mode 000 does not make a file unreadable here")
				}
			}
			root := t.TempDir()
			dirs := map[string]string{"state": filepath.Join(root, "state"), "tmp": filepath.Join(root, "tmp"), "home": filepath.Join(root, "home")}
			for _, d := range dirs {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			args := append([]string{"-test.run=^TestJoinSecretNeverLeaks$", "--", "worker", "join", "--json", "--share-existing", "later"}, tc.args...)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], args...)
			cmd.Env = append(childEnv(), joinLeakChildEnv+"=1", "MINION_STATE_DIR="+dirs["state"],
				"TMPDIR="+dirs["tmp"], "TMP="+dirs["tmp"], "TEMP="+dirs["tmp"], "HOME="+dirs["home"], "USERPROFILE="+dirs["home"])
			cmd.Env = append(cmd.Env, tc.env...)
			var stdout lockedBuffer
			stderr := lockedBuffer{seen: make(chan struct{})}
			if tc.cancel {
				stderr.mark = joinLeakReadMark
			}
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			var stdinW io.WriteCloser
			if tc.cancel {
				stdinW, err = cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				cmd.Stdin = strings.NewReader(tc.stdin)
			}
			var gate *os.File
			if runtime.GOOS == "linux" {
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = w.Close() }()
				cmd.ExtraFiles = []*os.File{r}
				cmd.Env = append(cmd.Env, joinLeakGateEnv+"=1")
				gate = w
				defer func() { _ = r.Close() }()
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			for _, f := range cmd.ExtraFiles {
				_ = f.Close() // the child has its own copy
			}
			if gate != nil {
				// The child waits on the gate, so its command line can be
				// read; right after the exec it can still be empty.
				var cmdline []byte
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
					cmdline, err = os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", cmd.Process.Pid))
					if err != nil {
						t.Fatalf("read the child's command line: %v", err)
					}
					if bytes.Contains(cmdline, []byte("worker\x00join")) {
						break
					}
				}
				_ = gate.Close()
				if !bytes.Contains(cmdline, []byte("worker\x00join")) {
					_ = cmd.Wait()
					t.Fatalf("not the child's command line: %q", cmdline)
				}
				assertNoSecret(t, "the child's /proc/self/cmdline", cmdline, secrets)
			}
			if tc.cancel {
				// The token goes in, the pipe stays open: the join reads
				// and waits for the end that never comes, until interrupted.
				if _, err := io.WriteString(stdinW, tc.stdin); err != nil {
					t.Fatal(err)
				}
				select {
				case <-stderr.seen:
				case <-ctx.Done():
					t.Fatalf("the child never read stdin; stderr: %s", stderr.String())
				}
				time.Sleep(100 * time.Millisecond)
				if err := cmd.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
			}
			err := cmd.Wait()
			if stdinW != nil {
				_ = stdinW.Close()
			}
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			out, errText := stdout.String(), stderr.String()
			if code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, out, errText)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(out), &m); err != nil {
				t.Fatalf("stdout is not one JSON object: %q (%v)", out, err)
			}
			if want := fmt.Sprintf("join-leak-child: invites=%d\n", tc.invites); !strings.Contains(errText, want) {
				t.Fatalf("want %q in stderr: %s", want, errText)
			}
			forbidden := secrets
			if tc.name == "malformed" {
				forbidden = map[string][]byte{"malformed input": []byte(malformed)}
				for k, v := range secrets {
					forbidden[k] = v
				}
			}
			assertNoSecret(t, "stdout", []byte(out), forbidden)
			assertNoSecret(t, "stderr", []byte(errText), forbidden)
			for _, p := range []string{tokenFile, unreadable, missing, inputs} {
				if strings.Contains(out+errText, p) {
					t.Errorf("the output names the path of the token file %s", p)
				}
			}
			for name, d := range dirs {
				scanNoSecret(t, name, d, forbidden)
			}
		})
	}
}

// childEnv is the environment of this process without MINION_* variables,
// so that the child sees only the settings of its case.
func childEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MINION_") {
			env = append(env, kv)
		}
	}
	return env
}

func assertNoSecret(t *testing.T, where string, data []byte, secrets map[string][]byte) {
	t.Helper()
	for name, s := range secrets {
		if len(s) > 0 && bytes.Contains(data, s) {
			t.Errorf("%s holds the %s", where, name)
		}
	}
}

// scanNoSecret checks every regular file under dir.
func scanNoSecret(t *testing.T, name, dir string, secrets map[string][]byte) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		assertNoSecret(t, name+" dir file "+rel, data, secrets)
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", name, err)
	}
}

// A FIFO without a writer must not hang the open: Ctrl+C ends it, and a FIFO
// is not accepted as a token file.
func TestSecretFileFIFONotBlocking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFO")
	}
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscallMkfifo(p); err != nil {
		t.Skip("mkfifo:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := readSecretInput(ctx, p, nil, 100, "the join token"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, errSecretInterrupted) {
			t.Fatalf("err = %v, want interrupted", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readSecretInput hung on a FIFO without writer")
	}
}

func TestSecretFileRejectsNonRegular(t *testing.T) {
	_, err := readSecretInput(context.Background(), t.TempDir(), nil, 100, "the join token")
	if err == nil {
		t.Fatal("a directory was accepted")
	}
}
