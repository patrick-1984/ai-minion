// Command headlesscheck is the headless gate of the tray work (ticket R1, owner
// rule R7): the minion binary, the deb/rpm packages, the container image and
// the armv7 build stay free of GUI modules. It lists the package graph of
// ./cmd/minion (go list -deps, CGO_ENABLED=0) for every release target of the
// Makefile (CROSS_TARGETS) and for every platform of the container image
// (.ko.yaml), and fails when a package of a forbidden GUI module is in it:
//
//	forbidden GUI dependency fyne.io/systray in <importing package>
//
// The GUI modules may only be imported by cmd/minion-tray and
// internal/tray/adapter (and godbus by test/traye2e); depguard in
// .golangci.yml enforces the same rule per file.
//
// Usage: go run ./scripts/headlesscheck [-dir repo-root] [package]
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// forbiddenModules are the GUI modules of the tray (and the D-Bus library its
// Linux adapter uses). A package belongs to one when its import path is the
// module path or below it.
var forbiddenModules = []string{
	"fyne.io/systray",
	"github.com/godbus/dbus",
	"github.com/ncruces/zenity",
}

// target is one build context of the gate.
type target struct {
	name                string // label in messages, for example "linux/arm/7" or "container linux/arm64"
	goos, goarch, goarm string
}

// releaseTargets are the Makefile CROSS_TARGETS (the 7 Worker builds).
var releaseTargets = []target{
	{name: "linux/amd64", goos: "linux", goarch: "amd64"},
	{name: "linux/arm64", goos: "linux", goarch: "arm64"},
	{name: "linux/arm/7", goos: "linux", goarch: "arm", goarm: "7"},
	{name: "darwin/amd64", goos: "darwin", goarch: "amd64"},
	{name: "darwin/arm64", goos: "darwin", goarch: "arm64"},
	{name: "windows/amd64", goos: "windows", goarch: "amd64"},
	{name: "windows/arm64", goos: "windows", goarch: "arm64"},
}

// containerTargets are the platforms of the container image (.ko.yaml and the
// kos section of .goreleaser.yaml), built from the same package.
var containerTargets = []target{
	{name: "container linux/amd64", goos: "linux", goarch: "amd64"},
	{name: "container linux/arm64", goos: "linux", goarch: "arm64"},
	{name: "container linux/arm/v7", goos: "linux", goarch: "arm", goarm: "7"},
}

// allTargets is every build context the gate checks.
func allTargets() []target {
	return append(append([]target(nil), releaseTargets...), containerTargets...)
}

// forbiddenModule returns the forbidden module that pkg belongs to, or "".
func forbiddenModule(pkg string) string {
	for _, m := range forbiddenModules {
		if pkg == m || strings.HasPrefix(pkg, m+"/") {
			return m
		}
	}
	return ""
}

// listDeps returns every package in the graph of pkg for t with the packages
// each one imports ("go list -deps", CGO_ENABLED=0, run in dir).
func listDeps(ctx context.Context, dir, pkg string, t target) (map[string][]string, error) {
	// -mod=readonly on the command line (it wins over GOFLAGS): the gate
	// certifies the module graph as go.mod and go.sum declare it, the way the
	// release build resolves it, and never adds or repairs a requirement. It
	// is also the one -mod value a Go workspace accepts.
	cmd := exec.CommandContext(ctx, "go", "list", "-mod=readonly", "-deps", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+t.goos, "GOARCH="+t.goarch, "GOARM="+t.goarm)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list -deps %s for %s: %w: %s", pkg, t.name, err, strings.TrimSpace(stderr.String()))
	}
	graph := map[string][]string{}
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		graph[f[0]] = f[1:]
	}
	return graph, sc.Err()
}

// violations returns one message per forbidden package in graph, naming the
// package that imports it ("forbidden GUI dependency <module> in <pkg>"),
// sorted and without duplicates.
func violations(graph map[string][]string) []string {
	seen := map[string]bool{}
	for pkg, imports := range graph {
		if forbiddenModule(pkg) != "" {
			continue // inside the GUI module itself: report its importer only
		}
		for _, imp := range imports {
			if m := forbiddenModule(imp); m != "" {
				seen[fmt.Sprintf("forbidden GUI dependency %s in %s", m, pkg)] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// check runs the gate for pkg in dir over targets. It returns an error that
// lists every violation with its target, or nil.
func check(ctx context.Context, dir, pkg string, targets []target) error {
	var errs []error
	for _, t := range targets {
		graph, err := listDeps(ctx, dir, pkg, t)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(graph) == 0 {
			errs = append(errs, fmt.Errorf("go list -deps %s for %s listed no package", pkg, t.name))
			continue
		}
		for _, v := range violations(graph) {
			errs = append(errs, fmt.Errorf("%s (%s)", v, t.name))
		}
	}
	return errors.Join(errs...)
}

func main() {
	dir := flag.String("dir", ".", "repository root (or any module directory)")
	flag.Parse()
	pkg := "./cmd/minion"
	if flag.NArg() > 0 {
		pkg = flag.Arg(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := check(ctx, *dir, pkg, allTargets()); err != nil {
		fmt.Fprintln(os.Stderr, "headlesscheck:", strings.ReplaceAll(err.Error(), "\n", "\nheadlesscheck: "))
		os.Exit(1)
	}
	fmt.Printf("headlesscheck: ok: %s has no GUI dependency on %d targets\n", pkg, len(allTargets()))
}
