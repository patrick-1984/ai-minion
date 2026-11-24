package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain runs the tests of this package with a private HOME, so that no
// test can change the startup files, the login registration, the Claude
// settings or the Worker state of the account that runs them (the installer
// that a test runs writes the shell login blocks of the HOME it sees). The Go
// tool keeps its caches and settings, for tests that build a binary.
func TestMain(m *testing.M) {
	os.Exit(runWithPrivateHome(m))
}

func runWithPrivateHome(m *testing.M) int {
	realHome, _ := os.UserHomeDir()
	cache, _ := os.UserCacheDir()
	cfg, _ := os.UserConfigDir()
	keep := map[string]string{}
	if realHome != "" {
		keep["GOPATH"] = filepath.Join(realHome, "go")
	}
	if cache != "" {
		keep["GOCACHE"] = filepath.Join(cache, "go-build")
	}
	if cfg != "" {
		keep["GOENV"] = filepath.Join(cfg, "go", "env")
	}
	for k, v := range keep {
		if _, ok := os.LookupEnv(k); !ok {
			_ = os.Setenv(k, v)
		}
	}
	if _, ok := os.LookupEnv("GOMODCACHE"); !ok {
		_ = os.Setenv("GOMODCACHE", filepath.Join(filepath.SplitList(os.Getenv("GOPATH"))[0], "pkg", "mod"))
	}
	home, err := os.MkdirTemp("", "minion-cli-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(home) }()
	_ = os.Setenv("HOME", home)
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "ZDOTDIR"} {
		_ = os.Unsetenv(k)
	}
	return m.Run()
}
