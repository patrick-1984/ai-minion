//go:build darwin && !cgo

package main

import "errors"

// The macOS menu bar needs cgo; the release builds this binary on a macOS
// runner with CGO_ENABLED=1. This stub only lets `go vet` pass in a
// cross-build without cgo.
func runTray() error {
	return errors.New("this build has no menu bar support (built without cgo)")
}
