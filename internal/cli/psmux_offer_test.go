package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
)

func psmuxOfferRig(t *testing.T, input string) (*cobra.Command, *config.Resolver, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetIn(strings.NewReader(input))
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetContext(context.Background())
	env := map[string]string{"MINION_STATE_DIR": dir}
	res := &config.Resolver{Registry: config.Default, Role: config.ScopeWorker,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	return cmd, res, out, dir
}

func offerDeps(found *bool, installs *int) psmuxOfferDeps {
	return psmuxOfferDeps{goos: "windows",
		interactive: func(io.Reader, io.Writer) bool { return true },
		findPsmux: func() string {
			if *found {
				return `C:\psmux.exe`
			}
			return ""
		},
		winget: func(context.Context, io.Writer, io.Writer) error { *installs++; *found = true; return nil }}
}

func savedOffer(t *testing.T, dir string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, workerConfigName))
	return string(b)
}

func TestPsmuxOfferYesInstallsWithWinget(t *testing.T) {
	cmd, res, out, dir := psmuxOfferRig(t, "\n") // Enter = yes
	found, installs := false, 0
	offerPsmux(cmd, res, offerDeps(&found, &installs))
	if installs != 1 || !strings.Contains(out.String(), "winget install psmux") || !strings.Contains(savedOffer(t, dir), "installed") {
		t.Fatalf("installs=%d out=%s saved=%s", installs, out, savedOffer(t, dir))
	}
}

func TestPsmuxOfferNoIsRememberedAndNotAskedAgain(t *testing.T) {
	cmd, res, _, dir := psmuxOfferRig(t, "n\n")
	found, installs := false, 0
	offerPsmux(cmd, res, offerDeps(&found, &installs))
	if installs != 0 || !strings.Contains(savedOffer(t, dir), "declined") {
		t.Fatalf("installs=%d saved=%s", installs, savedOffer(t, dir))
	}
	// The next start reads the saved answer and asks nothing.
	cmd2, res2, out2, _ := psmuxOfferRig(t, "y\n")
	file, err := config.LoadFile(filepath.Join(dir, workerConfigName))
	if err != nil {
		t.Fatal(err)
	}
	res2.File = file
	res2.LookupEnv = res.LookupEnv
	offerPsmux(cmd2, res2, offerDeps(&found, &installs))
	if installs != 0 || out2.Len() != 0 {
		t.Fatalf("asked again: %q", out2)
	}
}

func TestPsmuxOfferOnlyOnWindowsConsolesWithoutPsmux(t *testing.T) {
	found, installs := false, 0
	for name, mut := range map[string]func(*psmuxOfferDeps){
		"linux":         func(d *psmuxOfferDeps) { d.goos = "linux" },
		"not a console": func(d *psmuxOfferDeps) { d.interactive = func(io.Reader, io.Writer) bool { return false } },
		"already there": func(d *psmuxOfferDeps) { d.findPsmux = func() string { return `C:\psmux.exe` } },
	} {
		cmd, res, out, _ := psmuxOfferRig(t, "y\n")
		d := offerDeps(&found, &installs)
		mut(&d)
		offerPsmux(cmd, res, d)
		if installs != 0 || out.Len() != 0 {
			t.Errorf("%s: asked (%q)", name, out)
		}
	}
}
