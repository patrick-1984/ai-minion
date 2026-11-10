package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// appDir is the directory name Minion uses under every OS base directory.
const appDir = "minion"

// PathEnv carries everything the default-directory logic reads from the
// machine, so that the rules can be tested for every OS on any host.
type PathEnv struct {
	// GOOS is "linux", "darwin" or "windows"; other values follow the Linux
	// (XDG) rules.
	GOOS string
	// Getenv reads an environment variable.
	Getenv func(string) string
	// HomeDir returns the user's home directory.
	HomeDir func() (string, error)
	// TempDir returns the system temporary directory.
	TempDir func() string
	// UID is the numeric user ID (used to keep per-user runtime fallbacks
	// apart); it is 0 on Windows.
	UID int
}

// SystemPathEnv returns the PathEnv of the running process.
func SystemPathEnv() PathEnv {
	return PathEnv{
		GOOS:    runtime.GOOS,
		Getenv:  os.Getenv,
		HomeDir: os.UserHomeDir,
		TempDir: os.TempDir,
		UID:     os.Getuid(),
	}
}

// DataDir returns the default directory for durable data (the Hub database,
// AD-16): XDG_DATA_HOME or ~/.local/share on Linux, ~/Library/Application
// Support on macOS, %LOCALAPPDATA% on Windows, each plus "minion" (and
// "data" on Windows).
func (p PathEnv) DataDir() (string, error) {
	switch p.GOOS {
	case "darwin":
		return p.homeJoin("Library", "Application Support", appDir)
	case "windows":
		return p.windowsBase("data")
	default:
		return p.xdg("XDG_DATA_HOME", ".local", "share")
	}
}

// StateDir returns the default directory for Worker state (the Worker
// database, config file and logs, AD-16): XDG_STATE_HOME or ~/.local/state on
// Linux, ~/Library/Application Support on macOS, %LOCALAPPDATA% on Windows,
// each plus "minion" (and "state" on Windows).
func (p PathEnv) StateDir() (string, error) {
	switch p.GOOS {
	case "darwin":
		return p.homeJoin("Library", "Application Support", appDir)
	case "windows":
		return p.windowsBase("state")
	default:
		return p.xdg("XDG_STATE_HOME", ".local", "state")
	}
}

// RuntimeDir returns the default directory for sockets and other runtime
// files: $XDG_RUNTIME_DIR/minion on Linux when set, otherwise a per-user
// directory under the temporary directory; %LOCALAPPDATA%\minion\run on
// Windows.
func (p PathEnv) RuntimeDir() (string, error) {
	if p.GOOS == "windows" {
		return p.windowsBase("run")
	}
	if p.GOOS != "darwin" {
		if d := p.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(d) {
			return filepath.Join(d, appDir), nil
		}
	}
	tmp := os.TempDir()
	if p.TempDir != nil {
		tmp = p.TempDir()
	}
	return filepath.Join(tmp, appDir+"-"+strconv.Itoa(p.UID)), nil
}

// xdg returns $<env>/minion when the variable is an absolute path (the XDG
// rule: a relative value is ignored), else ~/<fallback...>/minion.
func (p PathEnv) xdg(env string, fallback ...string) (string, error) {
	if d := p.Getenv(env); filepath.IsAbs(d) {
		return filepath.Join(d, appDir), nil
	}
	return p.homeJoin(append(fallback, appDir)...)
}

func (p PathEnv) windowsBase(sub string) (string, error) {
	if d := p.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, appDir, sub), nil
	}
	return p.homeJoin("AppData", "Local", appDir, sub)
}

func (p PathEnv) homeJoin(elem ...string) (string, error) {
	home, err := p.HomeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errors.New("cannot determine the home directory; set MINION_DATA_DIR and MINION_STATE_DIR")
	}
	return filepath.Join(append([]string{home}, elem...)...), nil
}
