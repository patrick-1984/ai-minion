package cli

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestVersionCommandPrintsBuildInfo(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Execute(context.Background(), []string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, errb.String())
	}
	for _, want := range []string{
		"version: dev\n",
		"commit: dev\n",
		"built: dev\n",
		"go: " + runtime.Version() + "\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not contain %q", out.String(), want)
		}
	}
}

func TestVersionCommandRejectsArguments(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Execute(context.Background(), []string{"version", "extra"}, &out, &errb); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}
