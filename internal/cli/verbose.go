package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/obs"
	"github.com/patrick-1984/ai-minion/internal/obs/traffic"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// flagVerbose is the flag of the verbose setting ([config.KeyVerbose]). It is
// registered once, as a persistent flag of the root, so that every command
// accepts -v and --verbose; addSettingFlags leaves it out for that reason.
const flagVerbose = "verbose"

// addVerboseFlag adds -v/--verbose to root for every command.
func addVerboseFlag(root *cobra.Command) {
	root.PersistentFlags().BoolP(flagVerbose, "v", false,
		"verbose: log at debug level for troubleshooting; an explicit --log-level or MINION_LOG_LEVEL wins (env MINION_VERBOSE)")
}

// verboseFlag returns the value of -v/--verbose when it was given to cmd, and
// nil when it was not: an explicit --verbose=false must win over
// MINION_VERBOSE, so "false" and "not given" are kept apart.
func verboseFlag(cmd *cobra.Command) *bool {
	f := cmd.Flags().Lookup(flagVerbose)
	if f == nil || !f.Changed {
		return nil
	}
	on := f.Value.String() == "true"
	return &on
}

// commandLogger builds the logger of a command from log_level, log_format
// and the verbose switch ([config.ResolveLogLevel]), writing to out.
func commandLogger(cmd *cobra.Command, res *config.Resolver, out io.Writer) (*slog.Logger, error) {
	level, err := config.ResolveLogLevel(res, verboseFlag(cmd))
	if err != nil {
		return nil, err
	}
	format, err := res.Resolve(config.KeyLogFormat)
	if err != nil {
		return nil, err
	}
	return logx.New(logx.Options{Level: level, Format: logx.Format(format.Value), Output: out})
}

// workerLogger is commandLogger for a running Worker: its level can be
// switched at runtime (remote debug, worker_debug) and its recent lines are
// kept in memory (worker_logs).
func workerLogger(cmd *cobra.Command, res *config.Resolver, out io.Writer) (*slog.Logger, *logx.Live, error) {
	level, err := config.ResolveLogLevel(res, verboseFlag(cmd))
	if err != nil {
		return nil, nil, err
	}
	format, err := res.Resolve(config.KeyLogFormat)
	if err != nil {
		return nil, nil, err
	}
	return logx.NewLive(logx.Options{Level: level, Format: logx.Format(format.Value), Output: out}, logx.NewRing(0, 0))
}

// newWorkerSinks prepares the remote log sinks (syslog, OpenTelemetry) of a
// running Worker: the manager owns the remote slots of live's fan-out, and
// start builds the sinks once the Worker's lifecycle lock, update guard and
// enrollment are in place (the log sinks module calls it). A sink setting
// that does not validate never stops the Worker (a stricter check in a new
// release must not make an update look failed, nor keep a service down):
// both sinks stay off and the status and the local log say why; worker up
// refuses such a setting before it saves it. Standalone commands (CLI
// one-shots, minion mcp, worker pair) never call it: they log to stderr only.
func newWorkerSinks(res *config.Resolver, live *logx.Live, logger *slog.Logger, stateDir string) (w *workerSinks, start func(context.Context)) {
	w = &workerSinks{
		m:       obs.New(live.Fanout, obs.Process{Service: "minion-worker", Version: version.Version, InstanceID: ops.NewUUIDv7()}),
		traffic: traffic.New(traffic.SideWorker, live.Fanout),
	}
	m := w.m
	start = func(ctx context.Context) {
		if id := workerOTelID(ctx, stateDir); id != "" {
			m.SetProcess(obs.Process{Service: "minion-worker", Version: version.Version, InstanceID: id, WorkerID: id})
		}
		sinks, err := config.ResolveSinks(res, config.SinkFiles{ReadPrivate: readPrivateSinkFile})
		if err != nil {
			m.Disable(logx.ErrorText(err))
			logger.Error("log export is off: its settings are not valid", "error", logx.ErrorText(err))
			return
		}
		if err := m.Apply(ctx, sinks); err != nil {
			logger.Error("log export could not be started", "error", logx.ErrorText(err))
		}
		// The traffic log, like the sinks, never stops the Worker: a
		// setting that does not resolve leaves it off and says so.
		t, err := config.ResolveTraffic(res, sinks)
		if err != nil {
			logger.Error("the Worker traffic log is off: its settings are not valid", "error", logx.ErrorText(err))
			t = config.TrafficConfig{}
		}
		w.traffic.Configure(t)
	}
	return w, start
}

// workerSinks are the log sinks and the traffic log of a running Worker.
type workerSinks struct {
	m       *obs.Manager
	traffic *traffic.Log
}

// Status reports the sinks (minion worker status).
func (w *workerSinks) Status() []ops.LogSinkStatus { return w.m.Status() }

// Tap is the frame tap of the Worker's connection to its Hub.
func (w *workerSinks) Tap() transport.FrameTap { return w.traffic.Tap(traffic.Peer{}) }

// Shutdown writes what the traffic log has queued, then flushes and stops
// the sinks, all within ctx.
func (w *workerSinks) Shutdown(ctx context.Context) error {
	w.traffic.Close(ctx)
	return w.m.Shutdown(ctx)
}

// workerOTelID is the Worker id of the persisted enrollment, for the
// resource of its exported records (service.instance.id and
// minion.worker.id); "" when the Worker is not enrolled or the state cannot
// be read now.
func workerOTelID(ctx context.Context, stateDir string) string {
	enr, ok, err := readPairing(ctx, stateDir)
	if err != nil || !ok {
		return ""
	}
	return enr.WorkerID
}

// maxSinkFile bounds a private file of a sink setting (header file, TLS key).
const maxSinkFile = 1 << 20

// readPrivateSinkFile reads a secret file of a sink setting through
// identity.OpenPrivate: a regular file of this user that nobody else can
// read (Unix mode, Windows ACL), checked on the open handle.
func readPrivateSinkFile(path string) ([]byte, error) {
	f, err := identity.OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSinkFile+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s", path)
	}
	if len(b) > maxSinkFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxSinkFile)
	}
	return b, nil
}

// debugOpCall writes one debug line about an op command's call: the op, the
// backend (hub or local), the outcome (ok or the error code) and the
// duration. It writes nothing unless the level is debug (-v, MINION_VERBOSE
// or MINION_LOG_LEVEL=debug). Arguments and results are never logged.
func debugOpCall(cmd *cobra.Command, op ops.Op, local bool, start time.Time, err error) {
	res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: os.LookupEnv}
	log, lerr := commandLogger(cmd, res, cmd.ErrOrStderr())
	if lerr != nil || !log.Enabled(cmd.Context(), slog.LevelDebug) {
		return
	}
	backend := "hub"
	if local {
		backend = "local"
	}
	log.Debug("op call", "op", op.Name, "backend", backend, "outcome", outcomeCode(err),
		"duration", time.Since(start).String())
}

// outcomeCode is "ok" for nil, the code of a *proto.Error, else "error".
func outcomeCode(err error) string {
	if err == nil {
		return "ok"
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return string(pe.Code)
	}
	return "error"
}
