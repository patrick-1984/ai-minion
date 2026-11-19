package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func needGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go command is not on PATH")
	}
}

// The repository's minion binary has no GUI module on any release or
// container target.
func TestMinionIsHeadless(t *testing.T) {
	needGo(t)
	if testing.Short() {
		t.Skip("lists the package graph of 10 targets")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := check(ctx, root, "./cmd/minion", allTargets()); err != nil {
		t.Fatal(err)
	}
}

// The negative fixture imports fyne.io/systray: the gate fails with the exact
// message that names the module and the importing package.
func TestForbiddenFixtureFails(t *testing.T) {
	needGo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir, err := filepath.Abs("testdata/forbidden")
	if err != nil {
		t.Fatal(err)
	}
	err = check(ctx, dir, ".", []target{{name: "linux/arm/7", goos: "linux", goarch: "arm", goarm: "7"}})
	if err == nil {
		t.Fatal("the gate accepted a package that imports fyne.io/systray")
	}
	const want = "forbidden GUI dependency fyne.io/systray in example.com/forbidden (linux/arm/7)"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestTargetsCoverMakefile(t *testing.T) {
	var names []string
	for _, tg := range releaseTargets {
		names = append(names, tg.name)
	}
	const want = "linux/amd64 linux/arm64 linux/arm/7 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("release targets = %q, want the Makefile CROSS_TARGETS %q", got, want)
	}
	mk := readFile(t, "../../Makefile")
	if !strings.Contains(mk, "CROSS_TARGETS := "+want+"\n") {
		t.Fatal("the Makefile CROSS_TARGETS changed: update releaseTargets")
	}
	if !strings.Contains(mk, "check-headless") {
		t.Fatal("the Makefile has no check-headless target")
	}
}

func TestForbiddenModuleMatch(t *testing.T) {
	for pkg, want := range map[string]string{
		"fyne.io/systray":                       "fyne.io/systray",
		"fyne.io/systray/internal/generated":    "fyne.io/systray",
		"fyne.io/systrayx":                      "",
		"github.com/godbus/dbus/v5":             "github.com/godbus/dbus",
		"github.com/ncruces/zenity":             "github.com/ncruces/zenity",
		"github.com/patrick-1984/ai-minion/cli": "",
	} {
		if got := forbiddenModule(pkg); got != want {
			t.Errorf("forbiddenModule(%q) = %q, want %q", pkg, got, want)
		}
	}
}

// The gate certifies the module graph as declared: a fixture that imports a
// module its go.mod does not require fails, and the gate leaves go.mod as it
// was (it never resolves with -mod=mod), also when GOFLAGS asks for -mod=mod.
func TestMissingRequirementFailsReadOnly(t *testing.T) {
	needGo(t)
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOWORK", "off")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/missingreq")); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, filepath.Join(dir, "go.mod"))
	err := check(ctx, dir, ".", []target{{name: "linux/amd64", goos: "linux", goarch: "amd64"}})
	if err == nil {
		t.Fatal("the gate accepted a module graph with a missing requirement")
	}
	if after := readFile(t, filepath.Join(dir, "go.mod")); after != before {
		t.Fatalf("the gate changed go.mod:\n%s\nwas:\n%s", after, before)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.sum")); err == nil {
		t.Fatal("the gate wrote a go.sum")
	}
}

// Inside a Go workspace the gate runs (-mod=mod is the one value a workspace
// rejects) and still checks the packages.
func TestRunsInsideAGoWorkspace(t *testing.T) {
	needGo(t)
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":  "module example.test/ws\n\ngo 1.21\n",
		"go.work": "go 1.21\n\nuse .\n",
		"main.go": "package main\n\nfunc main() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := check(ctx, dir, ".", []target{{name: "linux/amd64", goos: "linux", goarch: "amd64"}}); err != nil {
		t.Fatalf("the gate failed inside a workspace: %v", err)
	}
}
