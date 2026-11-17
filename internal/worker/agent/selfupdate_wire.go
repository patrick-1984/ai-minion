package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/version"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// setupSelfUpdate runs the crash guard and registers worker_update on the
// executor. It returns the display facts of the hello, the feature list with
// self.update when the Worker can update itself, and the update module.
func setupSelfUpdate(log *slog.Logger, s Settings, ex *Executor, host term.SessionHost, hostKind string) (*selfUpdate, func() transport.WorkerFacts, func() []string) {
	resolve := func(key, def string) string {
		if s.Resolver == nil {
			return def
		}
		r, err := s.Resolver.Resolve(key)
		if err != nil || r.Value == "" {
			return def
		}
		return r.Value
	}
	exe, err := os.Executable()
	if err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
	} else {
		exe = ""
	}
	block := updateBlock(exe, resolve(config.KeyRemoteUpdate, "on"), os.Getenv, fileExists)

	u := &selfUpdate{log: log, stateDir: s.StateDir, exe: exe, block: block, now: time.Now,
		restart:      func() { restartProcess(log, exe) },
		workerID:     ex.Policy().WorkerID,
		holdSessions: ex.SetSessionCreateHold,
	}
	if rolled := lastRollback.Swap(nil); rolled != nil {
		u.setState(ops.UpdateStateRolledBack, rolled.To, rolled.From,
			"the new release did not start; the previous binary was restored")
		log.Warn("an update was rolled back to this release", "failed", rolled.To, "running", version.Version)
	}
	apiURL := resolve(config.KeyUpdateAPIURL, version.DefaultAPIURL)
	cacheDir := resolve(config.KeyUpdateCacheDir, filepath.Join(os.TempDir(), "minion-update"))
	u.installer = func() (releaseInstaller, error) {
		src, err := version.NewGitHubSource(apiURL, "minion/"+version.Version)
		if err != nil {
			return nil, err
		}
		return &version.Updater{
			Source:   src,
			Verifier: version.SigstoreVerifierFactory(version.TUFTrustedRoot{Dir: cacheDir}),
			Cache:    version.Cache{Dir: cacheDir},
			Out:      io.Discard,
		}, nil
	}
	if restartEndsSessions(runtime.GOOS, hostKind) && host != nil {
		// ConPTY sessions live inside the Worker process: a restart ends them.
		// psmux and tmux sessions outlive the Worker and never veto an update.
		u.sessionsLost = func(ctx context.Context) int {
			list, err := host.ListSessions(ctx)
			if err != nil {
				return 0
			}
			return len(list)
		}
	}
	u.register(ex)

	facts := func() transport.WorkerFacts {
		return transport.WorkerFacts{Build: version.Version, OS: runtime.GOOS, Arch: runtime.GOARCH, UpdateBlock: block}
	}
	features := func() []string {
		f := ex.Features()
		if block == "" {
			f = append(f, proto.FeatureSelfUpdate)
		}
		return f
	}
	return u, facts, features
}

// restartEndsSessions reports whether a restart of the Worker ends its
// sessions: only ConPTY sessions live inside the Worker process. A Windows
// Worker whose host is not known (a test core without a host spec) counts as
// ConPTY, as before v0.10.0.
func restartEndsSessions(goos, hostKind string) bool {
	if goos != "windows" {
		return false
	}
	return hostKind == "" || hostKind == transport.SessionHostConPTY
}

// lastRollback is the rollback RecoverUpdate found at this start, reported by
// worker_update_status.
var lastRollback atomic.Pointer[pendingRecord]

// connectorHubRange is the protocol range of the connected Hub, for the
// updater's compatibility check; nil while not connected.
func connectorHubRange(c *Connector) *version.ProtoRange {
	st := c.Status()
	if st.State != ConnConnected {
		return nil
	}
	lo, err1 := version.ParseProtoVersion(st.Welcome.Min.String())
	hi, err2 := version.ParseProtoVersion(st.Welcome.Max.String())
	if err1 != nil || err2 != nil {
		return nil
	}
	return &version.ProtoRange{Min: lo, Max: hi}
}
