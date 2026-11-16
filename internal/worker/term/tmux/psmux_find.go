package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
)

// FindPsmux returns the path of psmux.exe: from PATH, else from the WinGet
// links directory (%LOCALAPPDATA%\Microsoft\WinGet\Links), where "winget
// install psmux" puts it even before a new shell sees the changed PATH. It
// returns "" when psmux is not installed.
func FindPsmux() string {
	if p, err := exec.LookPath("psmux"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		p := filepath.Join(la, "Microsoft", "WinGet", "Links", "psmux.exe")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// NewPsmux returns a Host for the user's default psmux namespace with the
// psmux executable bin (3.4.0 or later).
func NewPsmux(bin string, opts ...Option) (*Host, error) {
	return New("", append([]Option{WithPsmux(), WithBinary(bin)}, opts...)...)
}
