package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Names of the managed host settings (Android Worker app).
const (
	// KeyTmuxBinary is the absolute path of the tmux executable every tmux
	// host factory of the Worker uses. Unset: "tmux" from PATH.
	KeyTmuxBinary = "tmux_binary"
	// KeyManagedHost names the supervisor that owns the tmux server: off
	// (the default) or android.
	KeyManagedHost = "managed_host"
	// KeyTmuxConfig is the fixed tmux configuration file of a managed host.
	KeyTmuxConfig = "tmux_config"
)

// Values of managed_host.
const (
	ManagedHostOff     = "off"
	ManagedHostAndroid = "android"
)

func init() { declareManagedHost(Default) }

func declareManagedHost(r *Registry) {
	r.Declare(Setting{
		Key:            KeyTmuxBinary,
		SupervisorOnly: true,
		Scope:          ScopeWorker,
		Help: "Absolute path of the tmux executable the Worker uses, for every tmux call (run, status, setup); " +
			"unset: tmux from PATH. The Android app passes it through the environment on every launch",
	})
	r.Declare(Setting{
		Key:            KeyManagedHost,
		SupervisorOnly: true,
		Scope:          ScopeWorker,
		Help:           "off (default) or android: a supervisor (the Android app) owns the tmux server. The Worker then runs every tmux command with -N and the fixed tmux_config, never starts a server, and reports its sessions as hidden",
		Default:        ManagedHostOff,
		Choices:        []string{ManagedHostOff, ManagedHostAndroid},
	})
	r.Declare(Setting{
		Key:            KeyTmuxConfig,
		SupervisorOnly: true,
		Scope:          ScopeWorker,
		Help:           "Absolute path of the fixed tmux configuration file of a managed host (managed_host android); the default is /dev/null",
	})
}

// ManagedHost is the resolved managed host spec. The zero value is an
// ordinary host: tmux from PATH and no supervisor.
type ManagedHost struct {
	// Android is true in the Android managed mode.
	Android bool
	// TmuxBinary is the absolute tmux executable; empty means PATH.
	TmuxBinary string
	// TmuxConfig is the fixed configuration file (managed mode only).
	TmuxConfig string
}

// ResolveManagedHost resolves tmux_binary, managed_host and tmux_config with
// res and validates them. The Android mode needs an absolute tmux binary.
func ResolveManagedHost(res *Resolver) (ManagedHost, error) {
	var m ManagedHost
	if res == nil {
		return m, nil
	}
	mode, err := res.Resolve(KeyManagedHost)
	if err != nil {
		return m, err
	}
	switch mode.Value {
	case "", ManagedHostOff:
	case ManagedHostAndroid:
		m.Android = true
	default:
		return m, fmt.Errorf("setting %q (%s): invalid value, expected %s or %s", KeyManagedHost, mode.Source, ManagedHostOff, ManagedHostAndroid)
	}
	bin, err := res.Resolve(KeyTmuxBinary)
	if err != nil {
		return m, err
	}
	if p := strings.TrimSpace(bin.Value); p != "" {
		if !filepath.IsAbs(p) {
			return m, fmt.Errorf("setting %q (%s): %q is not an absolute path", KeyTmuxBinary, bin.Source, p)
		}
		m.TmuxBinary, err = managedExecutable(filepath.Clean(p))
		if err != nil {
			return m, err
		}
	}
	cfg, err := res.Resolve(KeyTmuxConfig)
	if err != nil {
		return m, err
	}
	if p := strings.TrimSpace(cfg.Value); p != "" {
		if !filepath.IsAbs(p) {
			return m, fmt.Errorf("setting %q (%s): %q is not an absolute path", KeyTmuxConfig, cfg.Source, p)
		}
		m.TmuxConfig = filepath.Clean(p)
	}
	if m.Android && m.TmuxBinary == "" {
		return m, fmt.Errorf("setting %q is android: %q must name the absolute tmux binary", KeyManagedHost, KeyTmuxBinary)
	}
	return m, nil
}
