package presence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"github.com/patrick-1984/ai-minion/internal/config"
)

const (
	privateDirName = "presence"
	// PrivateDirName is the name of the private presence directory inside a
	// state directory.
	PrivateDirName   = privateDirName
	discoveryDirName = "presence-index"
	stageDirName     = ".stage"
	recordSuffix     = ".json"
	usageSuffix      = ".usage.json"
)

// ErrUnsupported is returned for a layout this OS cannot have (a root Worker
// on Windows).
var ErrUnsupported = errors.New("presence: not supported on this system")

// Layout says where one daemon publishes. Tests inject it; production code
// uses [DefaultLayout].
type Layout struct {
	// Scope is [ScopeUser] or [ScopeRoot].
	Scope string
	// UID is the effective uid of the daemon (-1 on Windows).
	UID int
	// RootUID is the uid that must own public files; 0 in production.
	RootUID int
	// StateDir is the Worker state directory of the profile.
	StateDir string
	// PrivateDir is the private presence directory (user scope).
	PrivateDir string
	// DiscoveryDir holds the owner-only pointer to PrivateDir; empty when the
	// state directory is the default one (user scope).
	DiscoveryDir string
	// MoreDiscoveryDirs are further owner-only pointer directories: the
	// account's login default, which differs from the service's own default
	// when the service runs with another XDG_STATE_HOME.
	MoreDiscoveryDirs []string
	// PublicRoot is the public presence directory (root scope).
	PublicRoot string
}

// PublicRootFor returns the public presence directory of goos, "" when there
// is none (Windows).
func PublicRootFor(goos string) string {
	switch goos {
	case "windows":
		return ""
	case "linux":
		return "/run/minion-presence"
	default:
		return "/var/run/minion-presence"
	}
}

// DefaultLayout returns the layout of the running process for stateDir.
func DefaultLayout(stateDir string) (Layout, error) {
	return layoutFor(stateDir, os.Geteuid(), runtime.GOOS, config.SystemPathEnv())
}

func layoutFor(stateDir string, uid int, goos string, env config.PathEnv) (Layout, error) {
	if stateDir == "" {
		return Layout{}, errors.New("presence: no state directory")
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return Layout{}, err
	}
	if goos != "windows" && uid == 0 {
		return Layout{Scope: ScopeRoot, UID: 0, RootUID: 0, StateDir: abs, PublicRoot: PublicRootFor(goos)}, nil
	}
	l := Layout{Scope: ScopeUser, UID: uid, StateDir: abs, PrivateDir: filepath.Join(abs, privateDirName)}
	env.GOOS = goos
	// The pointer goes to the service's own default and, on Linux, to the home
	// fallback a login shell without XDG_STATE_HOME uses. A service with a
	// private XDG_STATE_HOME would otherwise be invisible from a fresh login.
	var defaults []string
	if def, err := env.StateDir(); err == nil {
		defaults = append(defaults, def)
	}
	if goos == "linux" && env.HomeDir != nil {
		if home, err := env.HomeDir(); err == nil && home != "" {
			defaults = append(defaults, filepath.Join(home, ".local", "state", "minion"))
		}
	}
	seen := map[string]bool{filepath.Clean(abs): true}
	for _, def := range defaults {
		def = filepath.Clean(def)
		if seen[def] || !filepath.IsAbs(def) {
			continue
		}
		seen[def] = true
		dir := filepath.Join(def, discoveryDirName)
		if l.DiscoveryDir == "" {
			l.DiscoveryDir = dir
		} else {
			l.MoreDiscoveryDirs = append(l.MoreDiscoveryDirs, dir)
		}
	}
	return l, nil
}

func (l Layout) validate() error {
	switch l.Scope {
	case ScopeUser:
		if l.PrivateDir == "" || !filepath.IsAbs(l.PrivateDir) {
			return errors.New("presence: the private directory must be an absolute path")
		}
	case ScopeRoot:
		if runtime.GOOS == "windows" || l.PublicRoot == "" || !filepath.IsAbs(l.PublicRoot) {
			return ErrUnsupported
		}
		if l.UID != l.RootUID {
			return errors.New("presence: only root publishes in the public directory")
		}
		if filepath.Base(l.PublicRoot) == "minion" {
			return errors.New("presence: the socket directory is not a presence directory")
		}
	default:
		return errors.New("presence: unknown scope")
	}
	return nil
}

// discoveryDirs lists every pointer directory of the layout.
func (l Layout) discoveryDirs() []string {
	var out []string
	if l.DiscoveryDir != "" {
		out = append(out, l.DiscoveryDir)
	}
	return append(out, l.MoreDiscoveryDirs...)
}

// canonicalState is the state directory with every link resolved; when it does
// not exist (yet) the nearest existing parent is resolved and the rest kept.
func canonicalState(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if p, err := filepath.EvalSymlinks(abs); err == nil {
		return p
	}
	if p, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(p, filepath.Base(abs))
	}
	return abs
}

func hashKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// pathKey names the profile by its physical path: the name of the pointer
// entry.
func (l Layout) pathKey() string { return hashKey("path:" + canonicalState(l.StateDir)) }

// profileKey names the profile for the public claim: the device and inode of
// the physical state directory when it exists (so every alias of it, and a
// move, reach the same claim), the physical path otherwise.
func (l Layout) profileKey() string {
	phys := canonicalState(l.StateDir)
	if fi, err := os.Stat(phys); err == nil {
		if id := fileIdentity(fi); id != "" {
			return hashKey("id:" + id)
		}
	}
	return hashKey("path:" + phys)
}
