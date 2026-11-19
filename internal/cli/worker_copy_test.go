package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// fakeClipboard records the clipboard programs "worker copy" runs.
type fakeClipboard struct {
	installed map[string]bool
	fail      map[string]bool
	argv      [][]string
	stdin     []string
}

func (f *fakeClipboard) lookPath(name string) (string, error) {
	if f.installed[name] {
		return "/usr/bin/" + name, nil
	}
	return "", errors.New("not found")
}

func (f *fakeClipboard) run(_ context.Context, stdin []byte, name string, args ...string) error {
	f.argv = append(f.argv, append([]string{name}, args...))
	f.stdin = append(f.stdin, string(stdin))
	if f.fail[name] {
		return errors.New("exit status 1")
	}
	return nil
}

const (
	testHubWords    = "alpha bravo charlie delta echo foxtrot"
	testWorkerWords = "golf hotel india juliet kilo lima"
)

func copyDeps(goos string, env map[string]string, c *fakeClipboard) workerCopyDeps {
	return workerCopyDeps{
		goos: goos, lookupEnv: mapEnv(env), loadConfig: loadWorkerConfig,
		lookPath: c.lookPath, runStdin: c.run,
		hubKeyWords:    func(context.Context, string) (string, error) { return testHubWords, nil },
		workerKeyWords: func(string) (string, error) { return testWorkerWords, nil },
	}
}

func runCopy(t *testing.T, d workerCopyDeps, args ...string) (string, string, int) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerCopyCmd(d))
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, append([]string{"worker", "copy"}, args...), &out, &errOut)
	return out.String(), errOut.String(), code
}

// The value reaches the clipboard program only on its stdin, never in argv;
// the program of each OS is used with its fixed arguments, and a failing one
// gives way to the next.
func TestCopyUsesStdin(t *testing.T) {
	env := map[string]string{"MINION_STATE_DIR": t.TempDir()}
	for _, tc := range []struct {
		goos, key string
		installed []string
		fail      []string
		want      []string
		words     string
	}{
		{"windows", "hub-key", []string{"clip.exe"}, nil, []string{"clip.exe"}, testHubWords},
		{"darwin", "worker-key", []string{"pbcopy"}, nil, []string{"pbcopy"}, testWorkerWords},
		{"linux", "hub-key", []string{"wl-copy", "xclip"}, nil, []string{"wl-copy"}, testHubWords},
		{"linux", "hub-key", []string{"wl-copy", "xclip", "xsel"}, []string{"wl-copy"}, []string{"xclip", "-selection", "clipboard"}, testHubWords},
		{"linux", "worker-key", []string{"xsel"}, nil, []string{"xsel", "--clipboard", "--input"}, testWorkerWords},
	} {
		c := &fakeClipboard{installed: map[string]bool{}, fail: map[string]bool{}}
		for _, n := range tc.installed {
			c.installed[n] = true
		}
		for _, n := range tc.fail {
			c.fail[n] = true
		}
		out, _, code := runCopy(t, copyDeps(tc.goos, env, c), tc.key, "--json")
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("%v: %q: %v", tc, out, err)
		}
		if code != ExitOK || m["copied"] != true || m["program"] != tc.want[0] || m["key"] != tc.key {
			t.Fatalf("%v: %v, %d", tc, m, code)
		}
		if _, ok := m["value"]; ok || strings.Contains(out, tc.words) {
			t.Fatalf("%v: the value is in the JSON without --print: %q", tc, out)
		}
		last := len(c.argv) - 1
		if !slices.Equal(c.argv[last], tc.want) || c.stdin[last] != tc.words {
			t.Fatalf("%v: argv %v, stdin %q", tc, c.argv, c.stdin)
		}
		for _, a := range c.argv {
			for _, w := range a {
				if strings.Contains(w, tc.words) || strings.Contains(tc.words, w) {
					t.Fatalf("%v: the value is in argv %v", tc, a)
				}
			}
		}
	}
}

// Without a clipboard program the exit status is 1; the value is on stdout
// only with --print.
func TestCopyNoClipboard(t *testing.T) {
	env := map[string]string{"MINION_STATE_DIR": t.TempDir()}
	c := &fakeClipboard{}
	out, errOut, code := runCopy(t, copyDeps("linux", env, c), "hub-key")
	if code != ExitError || out != "" || strings.Contains(errOut, testHubWords) || len(c.argv) != 0 {
		t.Fatalf("no --print: %q, %q, %d", out, errOut, code)
	}
	out, _, code = runCopy(t, copyDeps("linux", env, c), "hub-key", "--print")
	if code != ExitError || out != testHubWords+"\n" {
		t.Fatalf("--print: %q, %d", out, code)
	}
	out, _, code = runCopy(t, copyDeps("linux", env, c), "hub-key", "--print", "--json")
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	if code != ExitError || m["ok"] != false || m["copied"] != false || m["value"] != testHubWords {
		t.Fatalf("--json --print: %v, %d", m, code)
	}
}

// A key that does not exist yet is exit 1 with nothing copied; a wrong
// argument is a usage error.
func TestCopyNoKeyAndUsage(t *testing.T) {
	env := map[string]string{"MINION_STATE_DIR": t.TempDir()}
	c := &fakeClipboard{installed: map[string]bool{"wl-copy": true}}
	d := copyDeps("linux", env, c)
	d.hubKeyWords = readHubKeyWords
	d.workerKeyWords = readWorkerKeyWords
	for _, key := range []string{"hub-key", "worker-key"} {
		out, errOut, code := runCopy(t, d, key, "--print")
		if code != ExitError || out != "" || len(c.argv) != 0 || !strings.Contains(errOut, "none") {
			t.Fatalf("%s: %q, %q, %d, %v", key, out, errOut, code, c.argv)
		}
	}
	if _, _, code := runCopy(t, d, "root-key"); code != ExitUsage {
		t.Fatalf("root-key: %d", code)
	}
	if _, _, code := runCopy(t, d); code != ExitUsage {
		t.Fatalf("no argument: %d", code)
	}
}
