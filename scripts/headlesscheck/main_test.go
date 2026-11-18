package main

import (
	"context"
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
