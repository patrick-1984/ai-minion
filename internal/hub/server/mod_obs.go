package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/obs"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// The obs module owns the remote log sinks (syslog, OpenTelemetry). It is the
// first module after the core: its sinks are built from the environment
// before any other module logs, and its stop hook is registered first, so it
// runs last and the sinks carry the stop of every other module. The logging
// module (after the settings module) then applies the panel: the live
// log_level and the sink settings, and their later changes.
const (
	obsModuleOrder     = OrderCore + 1
	loggingModuleOrder = settingsModuleOrder + 1

	// obsStopLimit bounds the flush of the sinks inside the Hub's shutdown
	// budget (DefaultShutdownTimeout, shared by the HTTP drain and every
	// module stop).
	obsStopLimit = 3 * time.Second
)

// ObsService is the Hub's handle on its log sinks. Manager is nil when the
// Hub runs without a runtime-controllable logger (Config.LogLive, tests).
type ObsService struct {
	Manager *obs.Manager
	live    *logx.Live
	verbose *bool
}

// obsSettingKeys are the Hub log export settings; all of them are live.
func obsSettingKeys() []string {
	var out []string
	for _, k := range config.ObsSettingKeys {
		if s, ok := config.Default.Lookup(k); ok && (s.Scope == config.ScopeHub || s.Scope == config.ScopeAny) {
			out = append(out, k)
		}
	}
	return out
}

func init() {
	DeclareLiveSetting(config.KeyLogLevel)
	for _, k := range obsSettingKeys() {
		DeclareLiveSetting(k)
	}
	RegisterModule("obs", obsModuleOrder, initObs, DeclaresService[*ObsService]())
	RegisterModule("logging", loggingModuleOrder, initLogging)
}

func initObs(ctx context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	svc := &ObsService{live: cfg.LogLive, verbose: cfg.Verbose}
	if err := Provide(env, svc); err != nil {
		return err
	}
	if cfg.LogLive == nil {
		return nil
	}
	m := obs.New(cfg.LogLive.Fanout, obs.Process{
		Service: "minion-hub", Version: version.Version, InstanceID: hubInstanceID(cfg.DataDir),
	})
	svc.Manager = m
	env.OnStop(func(ctx context.Context) error {
		sctx, cancel := obs.StopContext(ctx, obsStopLimit)
		defer cancel()
		return m.Shutdown(sctx)
	})
	// The environment alone: the panel is not open yet. A wrong environment
	// value fails the start, as for every other setting.
	sinks, err := config.ResolveSinks(&config.Resolver{Role: config.ScopeHub}, hubSinkFiles())
	if err != nil {
		return err
	}
	return m.Apply(ctx, sinks)
}

// hubSinkFiles reads the private files of the sink settings (TLS keys)
// through identity.OpenPrivate: owner and mode (Unix) or ACL (Windows) are
// checked on the open handle.
func hubSinkFiles() config.SinkFiles {
	return config.SinkFiles{ReadPrivate: readPrivate}
}

func readPrivate(path string) ([]byte, error) {
	f, err := identity.OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSinkFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSinkFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxSinkFile)
	}
	return b, nil
}

// maxSinkFile bounds a private file of a sink setting.
const maxSinkFile = 1 << 20

// hubInstanceID is the stable service.instance.id of this Hub: a random id
// kept in the data directory. "" when it cannot be read or written (the
// resource then has no instance id).
func hubInstanceID(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	p := filepath.Join(dataDir, "otel-instance-id")
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" && len(id) <= 64 {
			return id
		}
	}
	id := ops.NewUUIDv7()
	if err := os.WriteFile(p, []byte(id+"\n"), 0o600); err != nil {
		return ""
	}
	return id
}

func initLogging(ctx context.Context, env *Env) error {
	svc, err := Require[*ObsService](env)
	if err != nil {
		return err
	}
	settings, err := Require[*SettingsService](env)
	if err != nil {
		return err
	}
	if svc.live == nil {
		return nil
	}
	wireLogging(ctx, svc, settings, hubSinkFiles())
	return nil
}

// wireLogging applies the panel's log level and sink settings now and on
// every later change.
func wireLogging(ctx context.Context, svc *ObsService, settings *SettingsService, files config.SinkFiles) {
	log := slog.New(svc.live.Fanout.Local())
	applyLevel := func(_ context.Context, eff *config.Resolver) error {
		lvl, err := config.ResolveLogLevel(eff, svc.verbose)
		if err != nil {
			return err
		}
		l, err := logx.ParseLevel(lvl)
		if err != nil {
			return err
		}
		svc.live.SetBase(l)
		return nil
	}
	validateLevel := func(p *config.Resolver) error {
		_, err := config.ResolveLogLevel(p, svc.verbose)
		return err
	}
	if err := applyLevel(ctx, settings.Resolver()); err != nil {
		log.Warn("the log level of the panel is not valid; the start level stays", "error", err.Error())
	}
	settings.OnChange([]string{config.KeyLogLevel}, validateLevel, applyLevel)

	if svc.Manager == nil {
		return
	}
	applySinks := func(ctx context.Context, eff *config.Resolver) error {
		sinks, err := config.ResolveSinks(eff, files)
		if err != nil {
			return err
		}
		return svc.Manager.Apply(ctx, sinks)
	}
	validateSinks := func(p *config.Resolver) error {
		_, err := config.ResolveSinks(p, files)
		return err
	}
	// The panel may hold sink settings: apply them now. A stored
	// configuration that no longer works (a missing file) does not stop the
	// Hub; the sink stays off and the status says why.
	if err := applySinks(ctx, settings.Resolver()); err != nil {
		log.Error("log export could not be configured from the panel; the sink is off", "error", logx.ErrorText(err))
	}
	settings.OnChange(obsSettingKeys(), validateSinks, applySinks)
}
