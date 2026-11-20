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
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	otelsink "github.com/patrick-1984/ai-minion/internal/obs/otel"
	"github.com/patrick-1984/ai-minion/internal/obs/traffic"
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
	// Traffic is the Worker traffic log and Metrics the Worker traffic
	// metrics (both nil without a runtime logger).
	Traffic *traffic.Log
	Metrics *otelsink.Metrics
	// Compliance sends session content to the OpenTelemetry sink at the level
	// of compliance_monitoring; never nil, off until the settings are applied.
	Compliance *compliance.Monitor
	// proc is the identity of the Hub's exported data.
	proc    obs.Process
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
	svc.Compliance = compliance.New(func() compliance.Sink {
		if svc.Manager == nil {
			return nil
		}
		return svc.Manager.SessionSink()
	})
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
		if svc.Traffic != nil {
			svc.Traffic.Close(sctx) // queued traffic records first, bounded
		}
		if svc.Metrics != nil {
			_ = svc.Metrics.Shutdown(sctx)
		}
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
	svc.Traffic = traffic.New(traffic.SideHub, svc.live.Fanout)
	svc.proc = obs.Process{Service: "minion-hub", Version: version.Version, InstanceID: hubInstanceID(cfg.DataDir)}
	svc.Manager = obs.New(svc.live.Fanout, svc.proc)
	svc.Metrics = otelsink.NewMetrics()
	return wireLogging(ctx, svc, settings, hubSinkFiles())
}

// wireLogging applies the panel's log level and sink settings now and on
// every later change. A sink configuration that does not resolve leaves both
// sinks off (fail closed) with the reason in the status.
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
	// A sink configuration that does not resolve (from the panel or the
	// environment) never stops the Hub: both sinks stay off with the reason
	// in the status, and the settings page stays reachable to repair it.
	sinks, err := config.ResolveSinks(settings.Resolver(), files)
	if err != nil {
		m.Disable(logx.ErrorText(err))
		svc.Compliance.SetMode(compliance.Off)
		log.Error("log export could not be configured; both sinks are off", "error", logx.ErrorText(err))
	} else {
		if err := m.Apply(ctx, sinks); err != nil {
			log.Error("log export could not be started", "error", logx.ErrorText(err))
		}
		applyTraffic(svc, settings.Resolver(), sinks, log)
		applyMetrics(ctx, svc, settings.Resolver(), sinks, log)
		cm, cerr := config.ResolveCompliance(settings.Resolver(), sinks)
		if cerr != nil {
			log.Error("compliance monitoring is off: its settings are not valid", "error", logx.ErrorText(cerr))
			cm = config.ComplianceOff
		}
		setCompliance(svc, settings.Resolver(), cm)
		if r, err := settings.Resolver().Resolve(config.KeyCompliance); err == nil && cm != config.ComplianceOff && r.Source == config.SourceEnv {
			log.Warn("compliance monitoring is on, set by the environment", "level", cm, "variable", "MINION_COMPLIANCE_MONITORING")
		}
	}

	// The configuration the validator resolved is the one applied after the
	// commit (no second read of the files in between); changes are
	// serialised by the settings service.
	type resolved struct {
		sinks   config.Sinks
		traffic config.TrafficConfig
		metrics string
		comp    string
	}
	var mu sync.Mutex
	var prepared *resolved
	validateSinks := func(p *config.Resolver) error {
		s, err := config.ResolveSinks(p, files)
		var t config.TrafficConfig
		var lvl, cm string
		if err == nil {
			t, err = config.ResolveTraffic(p, s)
			if err != nil && unchangedAndInvalid(settings.Resolver(), p, files, trafficKeys, func(r *config.Resolver, s config.Sinks) error {
				_, e := config.ResolveTraffic(r, s)
				return e
			}) {
				// The traffic log is off already because of a setting that
				// this change does not touch (an environment one, say): it
				// must not keep the operator from stopping an export.
				t, err = config.TrafficConfig{}, nil
			}
		}
		if err == nil {
			lvl, err = config.ResolveMetricsLevel(p, s)
			if err != nil && unchangedAndInvalid(settings.Resolver(), p, files, []string{config.KeyOTelMetrics}, func(r *config.Resolver, s config.Sinks) error {
				_, e := config.ResolveMetricsLevel(r, s)
				return e
			}) {
				lvl, err = otelsink.MetricsOff, nil
			}
		}
		if err == nil {
			cm, err = config.ResolveCompliance(p, s)
			if err != nil && !s.OTLP.Enabled && complianceUnchanged(settings.Resolver(), p) {
				// Switching the sink off must not be refused because of a
				// compliance level the change does not touch: it stops capture.
				cm, err = config.ComplianceOff, nil
			}
		}
		mu.Lock()
		defer mu.Unlock()
		prepared = nil
		if err != nil {
			return err
		}
		prepared = &resolved{sinks: s, traffic: t, metrics: lvl, comp: cm}
		return nil
	}
	applySinks := func(ctx context.Context, eff *config.Resolver) error {
		mu.Lock()
		r := prepared
		prepared = nil
		mu.Unlock()
		if r == nil {
			s, err := config.ResolveSinks(eff, files)
			var t config.TrafficConfig
			var lvl, cm string
			if err == nil {
				t, err = config.ResolveTraffic(eff, s)
			}
			if err == nil {
				lvl, err = config.ResolveMetricsLevel(eff, s)
			}
			if err == nil {
				if cm, err = config.ResolveCompliance(eff, s); err != nil {
					cm, err = config.ComplianceOff, nil
				}
			}
			if err != nil {
				svc.Compliance.SetMode(compliance.Off)
				m.Disable(logx.ErrorText(err))
				if svc.Traffic != nil {
					svc.Traffic.Configure(config.TrafficConfig{})
				}
				if svc.Metrics != nil {
					_ = svc.Metrics.Configure(ctx, otelsink.MetricsConfig{}, nil)
				}
				return err
			}
			r = &resolved{sinks: s, traffic: t, metrics: lvl, comp: cm}
		}
		// The sinks and the traffic filter change together: nothing is
		// recorded between the two, so that no frame of a Worker that the
		// new filter excludes meets the new destination.
		if svc.Traffic != nil {
			svc.Traffic.Suspend()
		}
		// Capture stops first, and restarts on the new sink only after it is
		// in place: an observation never meets a half-changed configuration.
		svc.Compliance.SetMode(compliance.Off)
		if err := m.Apply(ctx, r.sinks); err != nil {
			if svc.Traffic != nil {
				svc.Traffic.Configure(r.traffic)
			}
			return err
		}
		setCompliance(svc, eff, r.comp)
		if svc.Traffic != nil {
			svc.Traffic.Configure(r.traffic)
		}
		return configureMetrics(ctx, svc, r.metrics, r.sinks.OTLP)
	}
	settings.OnChange(obsSettingKeys(), validateSinks, applySinks)
	return nil
}

// applyTraffic resolves and applies the traffic log at start; a setting that
// does not resolve leaves it off and says so locally.
func applyTraffic(svc *ObsService, res *config.Resolver, sinks config.Sinks, log *slog.Logger) {
	if svc.Traffic == nil {
		return
	}
	t, err := config.ResolveTraffic(res, sinks)
	if err != nil {
		log.Error("the Worker traffic log is off: its settings are not valid", "error", logx.ErrorText(err))
		t = config.TrafficConfig{}
	}
	svc.Traffic.Configure(t)
}

// applyMetrics resolves and applies the traffic metrics at start; a setting
// that does not resolve leaves them off and says so locally.
func applyMetrics(ctx context.Context, svc *ObsService, res *config.Resolver, sinks config.Sinks, log *slog.Logger) {
	if svc.Metrics == nil {
		return
	}
	lvl, err := config.ResolveMetricsLevel(res, sinks)
	if err != nil {
		log.Error("the Worker traffic metrics are off: their settings are not valid", "error", logx.ErrorText(err))
		lvl = otelsink.MetricsOff
	}
	if err := configureMetrics(ctx, svc, lvl, sinks.OTLP); err != nil {
		log.Error("the Worker traffic metrics could not be started", "error", logx.ErrorText(err))
	}
}

// configureMetrics applies the metrics level with the OpenTelemetry sink's
// endpoint, headers and trust.
func configureMetrics(ctx context.Context, svc *ObsService, level string, otlp config.OTLPConfig) error {
	if svc.Metrics == nil {
		return nil
	}
	res, _ := otelsink.NewResource(ctx, svc.proc)
	return svc.Metrics.Configure(ctx, otelsink.MetricsConfig{Level: level, OTLP: otlp}, res)
}

// setCompliance makes cm the effective mode and records whether the
// environment set it (a deployer's decision, shown in the status).
func setCompliance(svc *ObsService, res *config.Resolver, cm string) {
	svc.Compliance.SetMode(compliance.ParseMode(cm))
	origin := ""
	if r, err := res.Resolve(config.KeyCompliance); err == nil && r.Source == config.SourceEnv {
		origin = "the environment"
	}
	svc.Compliance.SetOrigin(origin)
}

// complianceUnchanged reports whether the proposed settings p leave
// compliance_monitoring as the running settings have it.
func complianceUnchanged(cur, p *config.Resolver) bool {
	a, ea := cur.Resolve(config.KeyCompliance)
	b, eb := p.Resolve(config.KeyCompliance)
	return ea == nil && eb == nil && a.Value == b.Value
}

var trafficKeys = []string{config.KeyTrafficLog, config.KeyTrafficLogTo, config.KeyTrafficLogWorkers}

// unchangedAndInvalid reports whether a feature (traffic log, metrics) that
// the proposed settings p cannot resolve is invalid in the same way in the
// running settings cur too: the keys it reads are the same in both, and cur
// does not resolve it either, so it is off already and p changes nothing
// about it.
func unchangedAndInvalid(cur, p *config.Resolver, files config.SinkFiles, keys []string,
	resolve func(*config.Resolver, config.Sinks) error) bool {
	sameErr := false
	for _, k := range keys {
		a, ea := cur.Resolve(k)
		b, eb := p.Resolve(k)
		if ea != nil || eb != nil {
			// The same unsupported value (an invalid environment enum, say)
			// on both sides is an unchanged input.
			if ea != nil && eb != nil && ea.Error() == eb.Error() {
				sameErr = true
				continue
			}
			return false
		}
		if a.Value != b.Value {
			return false
		}
	}
	if sameErr {
		// The value itself is unsupported, so the feature is off whatever
		// the sinks are: the sink files of the running settings are not
		// read (one may be gone while its sink keeps the trust it loaded).
		return true
	}
	cs, err := config.ResolveSinks(cur, files)
	return err == nil && resolve(cur, cs) != nil
}
