//go:build windows

package tmux

import "os"

// ownedByMe is false on Windows: psmux has no Unix socket to tighten.
func ownedByMe(os.FileInfo) bool { return false }
