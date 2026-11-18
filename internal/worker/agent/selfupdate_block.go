package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// updateBlock says why this Worker cannot update itself from the Hub, or ""
// when it can (Tailscale's "supported" check: the install method decides).
// The reason is shown on the Hub's Workers page.
func updateBlock(exe, remoteUpdate string, getenv func(string) string, _ func(string) bool) string {
	switch {
	case remoteUpdate == "off":
		return "remote updates are turned off on this Worker (remote_update=off)"
	case runsFromMinionImage(getenv, exe):
		// The Minion image (ko puts the binary in /ko-app): a new image, not a
		// new binary. A binary installed by hand inside any other container
		// updates like on a host (v0.9.3).
		return "runs from the Minion image: pull the new image"
	case exe == "/usr/bin/minion":
		return "installed by a package: upgrade it with apt or dnf"
	case exe == "":
		return "the path of the running binary is unknown"
	}
	if _, err := version.ParseVersion(version.Version); err != nil {
		return "a development build: update it by hand"
	}
	// The update writes the new binary next to the old one; it must be able to,
	// and on Windows the folder must be one no other account can change.
	dir := filepath.Dir(exe)
	f, err := os.CreateTemp(dir, ".minion-write-check-*")
	if err != nil {
		return "the folder of the binary is not writable by the Worker"
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	if runtime.GOOS == "windows" {
		if err := version.CheckTrustedExecutable(exe); err != nil {
			return "install minion in a folder that only administrators can change to update it remotely"
		}
	}
	return ""
}

// runsFromMinionImage reports whether the Worker runs from the Minion image:
// ko puts the binary in /ko-app, and the image sets MINION_IN_CONTAINER=1.
func runsFromMinionImage(getenv func(string) string, exe string) bool {
	return getenv("MINION_IN_CONTAINER") == "1" || strings.HasPrefix(exe, "/ko-app/")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
