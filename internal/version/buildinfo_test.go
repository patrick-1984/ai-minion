package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestDefaultsAreDev(t *testing.T) {
	// Tests are built without -ldflags, so the defaults must be visible.
	info := Get()
	if info.Version != "dev" || info.Commit != "dev" || info.Date != "dev" {
		t.Fatalf("defaults = %+v, want version, commit and date \"dev\"", info)
	}
}

func TestGoVersionIsRuntime(t *testing.T) {
	if got := Get().GoVersion; got != runtime.Version() {
		t.Fatalf("GoVersion = %q, want %q", got, runtime.Version())
	}
}

func TestInjectedValuesAreReported(t *testing.T) {
	oldV, oldC, oldD := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldV, oldC, oldD })
	Version, Commit, Date = "v1.2.3", "abc1234", "2026-11-09T10:00:00Z"

	out := Get().String()
	for _, want := range []string{
		"version: v1.2.3\n",
		"commit: abc1234\n",
		"built: 2026-11-09T10:00:00Z\n",
		"go: " + runtime.Version() + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("String() = %q, missing %q", out, want)
		}
	}
}
