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
	privateDirName   = "presence"
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
	if def, err := env.StateDir(); err == nil && filepath.Clean(def) != filepath.Clean(abs) {
		l.DiscoveryDir = filepath.Join(def, discoveryDirName)
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

func (l Layout) profileKey() string {
	h := sha256.Sum256([]byte(filepath.Clean(l.StateDir)))
	return hex.EncodeToString(h[:8])
}
