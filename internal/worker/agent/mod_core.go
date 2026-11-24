package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Module names and start orders. A later module (the local socket, the Hub
// connector) uses a higher order and gets the core services with [Require].
const (
	// ModuleCore opens the state, the terminal host and builds the executor.
	ModuleCore = "core"
	// OrderCore is the start order of the core module.
	OrderCore = 100
)

func init() {
	if err := RegisterCore(DefaultRegistry); err != nil {
		panic(err)
	}
}

// sessionProbeTimeout bounds the look at the sessions when the Worker starts.
const sessionProbeTimeout = 5 * time.Second

// coreDeps are the seams of the core module that tests replace.
type coreDeps struct {
	openStore func(ctx context.Context, stateDir string) (*store.Store, error)
	newHost   func(socket string) (term.SessionHost, error)
	// managedHost builds the tmux host when the managed host spec asks for
	// a given tmux binary or the Android managed mode; nil means
	// [newManagedTmuxHost].
	managedHost func(socket string, m config.ManagedHost) (term.SessionHost, error)
	// hostSpec turns the settings into the argument of newHost (the tmux
	// socket; on Windows psmux or ConPTY) and says whether its sessions are
	// hidden. Nil means the tmux socket, with an unknown host info.
	hostSpec   func(Settings) (string, SessionHostInfo, error)
	classifier Classifier
}

func defaultCoreDeps() coreDeps {
	return coreDeps{
		openStore:  func(ctx context.Context, dir string) (*store.Store, error) { return store.OpenContext(ctx, dir) },
		newHost:    newSessionHost,
		hostSpec:   sessionHostSpec,
		classifier: agentdetect.New(),
	}
}

// RegisterCore registers the core module on r: the Worker state, the terminal
// host on the resolved socket, the classifier, the local policy and the
// executor. It provides *store.Store, term.SessionHost, *Policy, *Executor and
// *SessionHostInfo to later modules.
func RegisterCore(r *Registry) error {
	return r.Register(ModuleCore, OrderCore, defaultCoreDeps().start)
}

// start opens the state, applies the settings of this run to the store and the
// policy (share_existing, preview_only) and provides the services.
func (d coreDeps) start(ctx context.Context, env *Env) (_ StopFunc, err error) {
	s := env.Settings()
	log := s.logger()

	st, err := d.openStore(ctx, s.StateDir)
	if err != nil {
		return nil, fmt.Errorf("open the Worker state in %s: %w", s.StateDir, err)
	}
	defer func() {
		if err != nil {
			_ = st.Close()
		}
	}()

	spec := s.TmuxSocket
	var hostInfo SessionHostInfo
	if d.hostSpec != nil {
		if spec, hostInfo, err = d.hostSpec(s); err != nil {
			return nil, fmt.Errorf("choose the terminal host: %w", err)
		}
	}
	if hostInfo.Kind == transport.SessionHostTmux {
		hostInfo.tmuxSocket, hostInfo.tmuxBinary = spec, s.Managed.TmuxBinary
	}
	var host term.SessionHost
	if s.Managed.Android || s.Managed.TmuxBinary != "" {
		mk := d.managedHost
		if mk == nil {
			mk = newManagedTmuxHost
		}
		host, err = mk(spec, s.Managed)
	} else {
		host, err = d.newHost(spec)
	}
	if err != nil {
		return nil, fmt.Errorf("start the terminal host: %w", err)
	}
	if l, ok := host.(interface{ SetLogger(*slog.Logger) }); ok {
		l.SetLogger(log) // debug lines of every listing (deduplicated)
	}
	defer func() {
		if err != nil {
			_ = closeHost(host)
		}
	}()

	// One look at the sessions now, so that a host that cannot list them is
	// reported to the Hub in the first hello (the event publisher keeps the
	// result fresh afterwards).
	health := &SessionListHealth{}
	probeCtx, cancelProbe := context.WithTimeout(ctx, sessionProbeTimeout)
	_, probeErr := host.ListSessions(probeCtx)
	cancelProbe()
	health.Set(probeErr)
	if probeErr != nil {
		log.Warn("cannot list the sessions of the terminal host", "error", probeErr.Error())
	}

	sel, err := applyShareExisting(ctx, st, s.ShareExisting)
	if err != nil {
		return nil, err
	}
	policy := NewPolicy(st)
	policy.SetPreviewOnly(s.PreviewOnly)
	policy.SetShareLock(s.ShareLock)
	if s.Resolver != nil {
		cfgPath := filepath.Join(s.StateDir, config.WorkerConfigFile)
		policy.SetNewSessionsCheck(func() bool { return config.ResolveAllowNewSessions(s.Resolver, cfgPath) })
	}
	policy.SetPipeAcceptFrom(s.PipeAcceptFrom)

	// The id the Hub gave this Worker, from its enrollment (the connector sets
	// it again from every welcome): share operations that name a Worker are held
	// to it, also while the Hub is not connected.
	if enr, err := st.LoadEnrollment(ctx); err == nil {
		policy.SetWorkerID(enr.WorkerID)
	} else if !errors.Is(err, store.ErrNotEnrolled) {
		return nil, fmt.Errorf("read the enrollment: %w", err)
	}

	ex, err := NewExecutor(Config{Host: host, Classifier: d.classifier, State: st, Policy: policy, Logger: log})
	if err != nil {
		return nil, err
	}
	ex.registerUsage(st)
	log.Info("Worker usage monitoring", "notice", ops.UsageDisclosure, "registration", "minion worker config usage")
	for _, p := range []func() error{
		func() error { return Provide(env, st) },
		func() error { return Provide(env, host) },
		func() error { return Provide(env, policy) },
		func() error { return Provide(env, ex) },
		func() error { return Provide(env, &hostInfo) },
		func() error { return Provide(env, health) },
	} {
		if err := p(); err != nil {
			return nil, err
		}
	}

	log.Info("worker core started",
		"name", s.Name, "state_dir", st.Dir(), "tmux_socket", s.TmuxSocket,
		"session_host", hostInfo.Kind, "managed_host", s.Managed.Android, "tmux_binary", s.Managed.TmuxBinary, "sessions_hidden", hostInfo.Hidden, "session_host_reason", hostInfo.localReason(),
		"share", string(sel.Mode), "preview_only", s.PreviewOnly, "share_lock", s.ShareLock,
		"pipe_accept_from", s.PipeAcceptFrom.String(), "call_api_key_file", s.CallAPIKeyFile != "")
	if !sel.Configured || sel.Mode == store.ShareLater {
		log.Warn("no tmux session is shared yet; sessions stay hidden until you choose, for example with --share-existing all")
	}
	return func(context.Context) error {
		// The host first: its sessions may still use the state.
		var errs []error
		if err := closeHost(host); err != nil {
			errs = append(errs, fmt.Errorf("close the terminal host: %w", err))
		}
		if err := st.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close the Worker state: %w", err))
		}
		return errors.Join(errs...)
	}, nil
}

// closeHost closes a terminal host that holds resources: the ConPTY host on
// Windows kills its programs and closes their pseudo consoles. The tmux host
// owns nothing, the sessions live in the tmux server.
func closeHost(h term.SessionHost) error {
	if c, ok := h.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// applyShareExisting saves the share mode named by share_existing and returns
// the selection in force. An empty value leaves the saved selection alone, so
// a choice made later (from the Hub or the wizard) survives a restart. The
// selected session names are kept for every mode.
func applyShareExisting(ctx context.Context, st *store.Store, value string) (store.ShareSelection, error) {
	sel, err := st.LoadShareContext(ctx)
	if err != nil {
		return sel, err
	}
	if value == "" {
		return sel, nil
	}
	mode, err := shareModeOf(value)
	if err != nil {
		return sel, err
	}
	if sel.Configured && sel.Mode == mode {
		return sel, nil
	}
	sel.Mode = mode
	if err := st.SaveShareContext(ctx, sel); err != nil {
		return sel, fmt.Errorf("apply %s: %w", config.KeyShareExisting, err)
	}
	sel.Configured = true
	return sel, nil
}

func shareModeOf(value string) (store.ShareMode, error) {
	switch value {
	case config.ShareExistingAll:
		return store.ShareAll, nil
	case config.ShareExistingNone:
		return store.ShareNone, nil
	case config.ShareExistingLater:
		return store.ShareLater, nil
	}
	return "", errors.New("share_existing must be all, none or later, got " + value)
}
