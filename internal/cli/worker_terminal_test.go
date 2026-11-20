package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
)

func TestWorkerUpChecksTmuxBeforeSetup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix tmux executable")
	}
	upgrade := "docs/install/worker-linux.md#upgrading-an-old-tmux"
	if runtime.GOOS == "darwin" {
		upgrade = "brew upgrade tmux"
	}
	for _, tc := range []struct {
		name, version, want, fix string
	}{
		{"missing", "", "tmux is missing from PATH", "apt-get install tmux"},
		{"too old", "tmux 3.0a", "tmux 3.2 or newer is required", upgrade},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			if err := os.WriteFile(filepath.Join(bin, "apt-get"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
				t.Fatal(err)
			}
			if tc.version != "" {
				if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\necho '"+tc.version+"'\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			f, _ := serviceGate(t, false, false)
			f.deps.setup.checkTerminal = checkTerminalHost
			out, err := f.run()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.fix) {
				t.Fatalf("unexpected error %v (output %q)", err, out)
			}
			if len(f.runCalls()) > 0 || strings.Contains(out, "what to share") {
				t.Fatalf("setup ran despite preflight failure: %q %v", out, f.events)
			}
		})
	}
}

func TestPsmuxPreflight(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	res := &config.Resolver{Registry: config.Default, Role: config.ScopeWorker, LookupEnv: os.LookupEnv}
	t.Setenv("MINION_SESSION_HOST", "auto")
	t.Setenv("MINION_PSMUX_OFFER", "ask")
	if err := checkPsmux(res); err != nil {
		t.Fatalf("missing psmux with session_host auto falls back to ConPTY: %v", err)
	}
	t.Setenv("MINION_PSMUX_OFFER", "declined")
	if err := checkPsmux(res); err != nil {
		t.Fatal("declined in a previous release:", err)
	}
	t.Setenv("MINION_SESSION_HOST", "psmux")
	if err := checkPsmux(res); err == nil || !strings.Contains(err.Error(), "winget install psmux") {
		t.Fatalf("explicit psmux is required even after a previous decline: %v", err)
	}
	t.Setenv("MINION_SESSION_HOST", "conpty")
	if err := checkPsmux(res); err != nil {
		t.Fatal("explicit ConPTY:", err)
	}

	if runtime.GOOS != "windows" {
		t.Setenv("MINION_SESSION_HOST", "psmux")
		for _, version := range []string{"3.3", "3.4"} {
			if err := os.WriteFile(filepath.Join(bin, "psmux"), []byte("#!/bin/sh\necho 'tmux 3.4'\necho 'psmux "+version+"'\n"), 0755); err != nil {
				t.Fatal(err)
			}
			err := checkPsmux(res)
			if version == "3.3" && (err == nil || !strings.Contains(err.Error(), "winget upgrade psmux")) {
				t.Fatalf("old psmux: %v", err)
			}
			if version == "3.4" && err != nil {
				t.Fatal("supported psmux:", err)
			}
		}
	}
}
