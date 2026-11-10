package version

import (
	"fmt"
	"runtime"
)

// Build information. The release pipeline and the Makefile overwrite these
// variables with -ldflags "-X <import path>.Version=..." (AD-24). A plain
// "go build" keeps the "dev" defaults.
var (
	// Version is the release version, for example "v0.1.0", or "dev".
	Version = "dev"
	// Commit is the git commit the binary was built from, or "dev".
	Commit = "dev"
	// Date is the build date in RFC 3339 UTC form, or "dev".
	Date = "dev"
)

// Info is a snapshot of the build information of the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
}

// Get returns the build information of the running binary.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
	}
}

// String renders the information as the multi-line text printed by
// "minion version".
func (i Info) String() string {
	return fmt.Sprintf("version: %s\ncommit: %s\nbuilt: %s\ngo: %s\n",
		i.Version, i.Commit, i.Date, i.GoVersion)
}
