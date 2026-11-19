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

// fakeOpener records the programs open-hub starts.
type fakeOpener struct {
	calls [][]string
	err   error
}

func (f *fakeOpener) run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.err
}

func openHubDeps(goos string, env map[string]string, o *fakeOpener) workerOpenHubDeps {
	return workerOpenHubDeps{goos: goos, lookupEnv: mapEnv(env), loadConfig: loadWorkerConfig, run: o.run}
}

func runOpenHub(t *testing.T, d workerOpenHubDeps, args ...string) (string, string, int) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerOpenHubCmd(d))
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, append([]string{"worker", "open-hub"}, args...), &out, &errOut)
	return out.String(), errOut.String(), code
}

// --print prints the browser URL of the Hub (no query or fragment; the
// configuration already refuses user info) and starts
// nothing.
func TestOpenHubPrint(t *testing.T) {
	o := &fakeOpener{}
	env := map[string]string{"MINION_STATE_DIR": t.TempDir(), "MINION_SERVER": "wss://hub.example/base/?token=secret#frag"}
	out, _, code := runOpenHub(t, openHubDeps("linux", env, o), "--print")
	if code != ExitOK || out != "https://hub.example/base/\n" || len(o.calls) != 0 {
		t.Fatalf("print = %q, %d, calls %v", out, code, o.calls)
	}
	out, _, code = runOpenHub(t, openHubDeps("linux", env, o), "--print", "--json")
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	if code != ExitOK || m["ok"] != true || m["url"] != "https://hub.example/base/" || m["opened"] != false || len(o.calls) != 0 {
		t.Fatalf("json = %v, %d, calls %v", m, code, o.calls)
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "frag") {
		t.Fatalf("credentials in %q", out)
	}
}

// Without --print the opener of the platform gets the URL as one argument
// (no shell); a failing opener is exit 1 and still names the URL.
func TestOpenHubFakeRunner(t *testing.T) {
	env := map[string]string{"MINION_STATE_DIR": t.TempDir(), "MINION_SERVER": "ws://hub.example:8080/m"}
	for goos, want := range map[string][]string{
		"linux":   {"xdg-open", "http://hub.example:8080/m"},
		"darwin":  {"open", "http://hub.example:8080/m"},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", "http://hub.example:8080/m"},
	} {
		o := &fakeOpener{}
		out, _, code := runOpenHub(t, openHubDeps(goos, env, o), "--json")
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("%s: %q: %v", goos, out, err)
		}
		if code != ExitOK || m["opened"] != true || len(o.calls) != 1 || !slices.Equal(o.calls[0], want) {
			t.Fatalf("%s: %v, %d, calls %v", goos, m, code, o.calls)
		}
	}
	o := &fakeOpener{err: errors.New("exec: \"xdg-open\": executable file not found in $PATH")}
	out, _, code := runOpenHub(t, openHubDeps("linux", env, o), "--json")
	if code != ExitError || !strings.Contains(out, `"url":"http://hub.example:8080/m"`) || !strings.Contains(out, `"opened":false`) {
		t.Fatalf("failing opener = %q, %d", out, code)
	}
}

// A Solo Worker has no Hub to open: exit 1, nothing started.
func TestOpenHubSolo(t *testing.T) {
	o := &fakeOpener{}
	_, errOut, code := runOpenHub(t, openHubDeps("linux", map[string]string{"MINION_STATE_DIR": t.TempDir()}, o))
	if code != ExitError || len(o.calls) != 0 || !strings.Contains(errOut, "no Hub URL") {
		t.Fatalf("solo = %q, %d, calls %v", errOut, code, o.calls)
	}
}
