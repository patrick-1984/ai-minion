package tmux

import (
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/config"
)

// DefaultManagedConfig is the fixed tmux configuration of a managed host when
// none is given: empty, so no file of the user is read.
const DefaultManagedConfig = "/dev/null"

// WithManaged marks the server as owned by a supervisor (the Android app
// service starts it in the foreground with the same socket and configuration).
// Every invocation of the adapter then passes -N, so that no command, such as
// a session_create of the Hub, starts a server that nobody supervises, and
// -f configFile, so that no configuration of the user is read. An empty
// configFile means [DefaultManagedConfig].
func WithManaged(configFile string) Option {
	return func(h *Host) {
		h.managed = true
		h.configFile = configFile
		if h.configFile == "" {
			h.configFile = DefaultManagedConfig
		}
	}
}

// Managed reports whether the host is a managed one (see [WithManaged]).
func (h *Host) Managed() bool { return h.managed }

// OptionsFor returns the options that every tmux host factory (the Worker, its
// status, the terminal preflight and the local commands) passes for the
// resolved managed host spec: the absolute tmux binary and, for the Android
// managed mode, -N and the fixed configuration. The zero spec gives none.
func OptionsFor(m config.ManagedHost) []Option {
	var opts []Option
	if m.TmuxBinary != "" {
		opts = append(opts, WithBinary(filepath.Clean(m.TmuxBinary)))
	}
	if m.Android {
		opts = append(opts, WithManaged(m.TmuxConfig))
	}
	return opts
}
