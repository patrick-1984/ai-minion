package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func initObs(_ context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	svc := &ObsService{live: cfg.LogLive, verbose: cfg.Verbose}
	if err := Provide(env, svc); err != nil {
		return err
	}
	// Registered first, so it runs last: the sinks carry the stop of every
	// other module. The sinks themselves start in the logging module, once
	// the data directory was checked and the panel can be read, so that only
	// the validated effective configuration ever sends.
	env.OnStop(func(ctx context.Context) error {
		if svc.Manager == nil {
			return nil
		}
		sctx, cancel := obs.StopContext(ctx, obsStopLimit)
		defer cancel()
		_ = svc.Manager.Shutdown(sctx) // a loss is written to the local log
		return nil
	})
	return nil
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
// kept in the data directory (checked by the store module before). An
// existing file is read without following a link and only when it is a
// small regular file; a new one is created exclusively. Anything unexpected
// gives "" (the resource then has no instance id) and nothing is rewritten.
func hubInstanceID(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	p := filepath.Join(dataDir, "otel-instance-id")
	if fi, err := os.Lstat(p); err == nil {
		if !fi.Mode().IsRegular() || fi.Size() > 128 {
			return ""
		}
		f, err := os.Open(p)
		if err != nil {
			return ""
		}
		defer func() { _ = f.Close() }()
		b, _ := io.ReadAll(io.LimitReader(f, 128))
		id := strings.TrimSpace(string(b))
		if id == "" || len(id) > 64 {
			return ""
		}
		return id
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	id := ops.NewUUIDv7()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ""
	}
	_, werr := f.WriteString(id + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil {
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
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	svc.Manager = obs.New(svc.live.Fanout, obs.Process{
		Service: "minion-hub", Version: version.Version, InstanceID: hubInstanceID(cfg.DataDir),
	})
	return wireLogging(ctx, svc, settings, hubSinkFiles())
}

// wireLogging applies the panel's log level and sink settings now and on
// every later change. A sink configuration that does not resolve leaves both
// sinks off (fail closed) with the reason in the status; it stops the Hub
// only when the environment alone is already invalid, as for every setting.
func wireLogging(ctx context.Context, svc *ObsService, settings *SettingsService, files config.SinkFiles) error {
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

	m := svc.Manager
	if m == nil {
		return nil
	}
	sinks, err := config.ResolveSinks(settings.Resolver(), files)
	if err != nil {
		envOnly := settings.Resolver()
		envOnly.Panel = nil
		if _, eerr := config.ResolveSinks(envOnly, files); eerr != nil {
			return eerr
		}
		m.Disable(logx.ErrorText(err))
		log.Error("log export could not be configured from the panel; both sinks are off", "error", logx.ErrorText(err))
	} else if err := m.Apply(ctx, sinks); err != nil {
		log.Error("log export could not be started", "error", logx.ErrorText(err))
	}

	// The configuration the validator resolved is the one applied after the
	// commit (no second read of the files in between); changes are
	// serialised by the settings service.
	var mu sync.Mutex
	var prepared *config.Sinks
	validateSinks := func(p *config.Resolver) error {
		s, err := config.ResolveSinks(p, files)
		mu.Lock()
		defer mu.Unlock()
		prepared = nil
		if err != nil {
			return err
		}
		prepared = &s
		return nil
	}
	applySinks := func(ctx context.Context, eff *config.Resolver) error {
		mu.Lock()
		s := prepared
		prepared = nil
		mu.Unlock()
		if s == nil {
			r, err := config.ResolveSinks(eff, files)
			if err != nil {
				m.Disable(logx.ErrorText(err))
				return err
			}
			s = &r
		}
		if err := m.Apply(ctx, *s); err != nil {
			return err
		}
		return nil
	}
	settings.OnChange(obsSettingKeys(), validateSinks, applySinks)
	return nil
}
