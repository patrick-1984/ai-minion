//go:build (darwin && !cgo) || (linux && arm)

package main

import "errors"

// No tray in this build: the macOS menu bar needs cgo (the release builds the
// tray on a macOS runner with CGO_ENABLED=1), and a 32-bit ARM Linux host
// (armv7, Raspberry Pi) is headless and never gets a tray. The stub keeps
// `go vet ./...` working for every target and links no GUI library.
func runTray() error {
	return errors.New("this build has no tray (no cgo on macOS, or armv7 Linux)")
}
