package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

func runDocsCmd(t *testing.T, d docsDeps, args ...string) (string, int) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newDocsCmd(d))
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, append([]string{"docs"}, args...), &out, &errOut)
	return out.String(), code
}

// --print prints the guide URL and starts nothing.
func TestDocsPrint(t *testing.T) {
	for _, tc := range []struct {
		goos string
		args []string
		want string
	}{
		{"linux", nil, docsBaseURL + "guides/tray.md"},
		{"linux", []string{"tray"}, docsBaseURL + "guides/tray.md"},
		{"linux", []string{"worker"}, docsBaseURL + "install/worker-linux.md"},
		{"darwin", []string{"worker"}, docsBaseURL + "install/worker-macos.md"},
		{"windows", []string{"worker"}, docsBaseURL + "install/worker-windows-preview.md"},
	} {
		o := &fakeOpener{}
		out, code := runDocsCmd(t, docsDeps{goos: tc.goos, run: o.run}, append([]string{"--print"}, tc.args...)...)
		if code != ExitOK || out != tc.want+"\n" || len(o.calls) != 0 {
			t.Fatalf("%v: %q, %d, calls %v", tc, out, code, o.calls)
		}
	}
	if docsBaseURL != "https://github.com/patrick-1984/ai-minion/blob/main/docs/" {
		t.Fatalf("base URL %q", docsBaseURL)
	}
	o := &fakeOpener{}
	if _, code := runDocsCmd(t, docsDeps{goos: "linux", run: o.run}, "--print", "hub"); code != ExitUsage {
		t.Fatalf("unknown topic: %d", code)
	}
}

// Without --print the opener of the platform gets the URL as one argument.
func TestDocsOpen(t *testing.T) {
	o := &fakeOpener{}
	out, code := runDocsCmd(t, docsDeps{goos: "darwin", run: o.run}, "--json")
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	want := []string{"open", docsBaseURL + "guides/tray.md"}
	if code != ExitOK || m["opened"] != true || m["url"] != want[1] || len(o.calls) != 1 || !slices.Equal(o.calls[0], want) {
		t.Fatalf("%v, %d, calls %v", m, code, o.calls)
	}
}
