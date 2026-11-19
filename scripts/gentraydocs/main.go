// Command gentraydocs writes the generated parity section of
// docs/guides/tray.md: the text between the markers
// trayactions.BeginMarker and trayactions.EndMarker is replaced by
// trayactions.ParityMarkdown(), the table of every tray control and its
// command line (W11). TestParityDocsFresh fails when the guide is stale; run
// this command after changing internal/trayactions.
//
// Usage: go run ./scripts/gentraydocs [repo-root]   (default: the current directory)
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/trayactions"
)

// guidePath is the tray guide, relative to the repository root.
const guidePath = "docs/guides/tray.md"

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	changed, err := generate(filepath.Join(root, guidePath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gentraydocs:", err)
		os.Exit(1)
	}
	if changed {
		fmt.Println("gentraydocs: updated", guidePath)
	} else {
		fmt.Println("gentraydocs: up to date")
	}
}

// generate splices the parity section into the guide at path and reports
// whether the file changed.
func generate(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	out, ok := trayactions.Splice(string(b))
	if !ok {
		return false, fmt.Errorf("%s lacks the markers %q and %q", path, trayactions.BeginMarker, trayactions.EndMarker)
	}
	if out == string(b) {
		return false, nil
	}
	return true, os.WriteFile(path, []byte(out), 0o644)
}
