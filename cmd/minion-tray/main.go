// Command minion-tray is the desktop tray for Windows, Ubuntu desktop and
// macOS. It is a separate binary: `minion` itself has no GUI dependency, and
// every tray action runs a `minion` command (see docs/guides/tray.md).
package main

import (
	"fmt"
	"os"

	"github.com/patrick-1984/ai-minion/internal/version"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("minion-tray " + version.Get().Version)
		return
	}
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: minion-tray [--version]")
		os.Exit(2)
	}
	if err := runTray(); err != nil {
		fmt.Fprintln(os.Stderr, "minion-tray:", err)
		os.Exit(1)
	}
}
