//go:build linux

package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/selfupdate/truenas"
)

func TestHubUpdaterRefusesNonRoot(t *testing.T) {
	cmd := newHubUpdaterCmd(hubUpdaterDeps{euid: func() int { return 1000 }})
	cmd.SetArgs(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "as root") {
		t.Fatalf("err = %v", err)
	}
}

func TestHubUpdaterValidatesConfigAndRuns(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.json")
	if err := os.WriteFile(cfg, []byte(`{"app":"ai-minion","dataset":"tank/hub","readiness_url":"http://127.0.0.1:1/readyz","enabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	built := false
	deps := hubUpdaterDeps{euid: func() int { return 0 }, ownerUID: os.Getuid(),
		build: func(c truenas.Config, j *truenas.Journal, _ *slog.Logger) *truenas.Executor {
			built = true
			if c.Window != "03-05" || !c.Enabled {
				t.Errorf("config = %+v", c)
			}
			return &truenas.Executor{Cfg: c, Journal: j}
		}}
	cmd := newHubUpdaterCmd(deps)
	cmd.SetArgs([]string{"--config", cfg, "--state-dir", filepath.Join(dir, "state"), "--enabled", "--clear-quarantine"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !built {
		t.Fatal("executor not built")
	}
	// A bad dataset (a mount path) is refused before anything runs.
	cmd = newHubUpdaterCmd(deps)
	cmd.SetArgs([]string{"--config", cfg, "--dataset", "/mnt/tank/hub", "--state-dir", filepath.Join(dir, "state")})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil {
		t.Fatal("mount path accepted as dataset")
	}
}
