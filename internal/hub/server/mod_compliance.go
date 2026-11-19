package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/obs"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	otelsink "github.com/patrick-1984/ai-minion/internal/obs/otel"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The compliance module owns the monitoring policy (its version, its gate and
// its audit rows) and the compliance exporter (A49 T15, T18). It runs after
// the settings, audit and logging modules and before the REST framework and
// the Worker modules, whose front doors take the Monitor it provides.
const complianceModuleOrder = loggingModuleOrder + 1

// Audit actions of the compliance rows (design §5.4). The target is
// Policy.AuditTarget: version, level, input, traffic and the destination
// label; never a credential or a content.
const (
	AuditComplianceEnable    = "compliance.enable"
	AuditComplianceChange    = "compliance.change"
	AuditComplianceRetarget  = "compliance.retarget"
	AuditComplianceDisable   = "compliance.disable"
	AuditComplianceStartup   = "compliance.startup"
	AuditComplianceEffective = "compliance.effective"
	AuditComplianceFailed    = "compliance.failed"
)

// ComplianceExporter is the running compliance export.
type ComplianceExporter interface {
	compliance.Sink
	// Status is the effective state of the export for the diagnostics.
	Status() ops.LogSinkStatus
	// Close flushes within ctx; what is left is dropped and counted.
	Close(ctx context.Context) error
	// Discard drops the queue (a replaced configuration): records queued under
	// a revoked destination or credential are never sent with the new one.
	Discard()
}

// ComplianceBuilder builds the exporter of a validated configuration.
type ComplianceBuilder func(cfg config.ComplianceConfig, proc obs.Process) (ComplianceExporter, error)

// ComplianceService is the Hub's handle on compliance monitoring.
type ComplianceService struct {
	Monitor *compliance.Monitor

	settings *SettingsService
	store    *store.Store
	aud      SettingsTxAuditor
	files    config.SinkFiles
	proc     obs.Process
	build    ComplianceBuilder
	log      *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	cfgDig  string
	exp     ComplianceExporter
	lastErr string
	gen     uint64
	// carry and carrySent are the counters of exporters that were replaced.
	carry     map[string]uint64
	carrySent uint64
}

// defaultComplianceBuilder builds the OTLP/HTTP exporter: its own provider,
// credentials, trust and queue (internal/obs/otel/compliance.go).
var defaultComplianceBuilder ComplianceBuilder = func(cfg config.ComplianceConfig, proc obs.Process) (ComplianceExporter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return otelsink.NewCompliance(ctx, cfg, proc)
}

func init() {
	for _, k := range config.ComplianceKeys {
		DeclareLiveSetting(k)
	}
	RegisterModule("compliance", complianceModuleOrder, initCompliance,
		DeclaresService[*ComplianceService](), DeclaresService[*compliance.Monitor]())
}

func initCompliance(ctx context.Context, env *Env) error {
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	settings, err := Require[*SettingsService](env)
	if err != nil {
		return err
	}
	aud, err := Require[SettingsAuditor](env)
	if err != nil {
		return err
	}
	txa, ok := aud.(SettingsTxAuditor)
	if !ok {
		return errors.New("compliance: the settings auditor cannot write in a transaction")
	}
	obsSvc, err := Require[*ObsService](env)
	if err != nil {
		return err
	}
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	svc := &ComplianceService{
		Monitor: compliance.NewMonitor(compliance.Deps{}), settings: settings, store: st, aud: txa,
		files: hubSinkFiles(), proc: obsSvc.proc, build: defaultComplianceBuilder, log: cfg.logger(), now: time.Now,
	}
	if obsSvc.proc.Service == "" {
		svc.proc = obs.Process{Service: "minion-hub", Version: "", InstanceID: hubInstanceID(cfg.DataDir)}
	}
	if obsSvc.Traffic != nil {
		obsSvc.Traffic.SetPayloadGate(func() bool { return svc.Monitor.Policy().TrafficPayload })
	}
	if err := Provide(env, svc); err != nil {
		return err
	}
	if err := Provide(env, svc.Monitor); err != nil {
		return err
	}
	// The gate and the policy: the keys can change only through
	// compliance_set; the policy version and the audit rows commit with the
	// settings, and the effective configuration is built after the commit.
	keys := config.GatedKeys()
	settings.OnChange(keys, svc.validate, svc.apply)
	settings.OnChangeTx(keys, svc.syncTx)
	if err := svc.start(ctx); err != nil {
		return err
	}
	env.OnStop(func(ctx context.Context) error {
		sctx, cancel := obs.StopContext(ctx, obsStopLimit)
		defer cancel()
		svc.Close(sctx)
		return nil
	})
	return nil
}

// validate refuses a proposed configuration that does not resolve, before
// anything is stored. The error names keys, never values.
func (s *ComplianceService) validate(p *config.Resolver) error {
	_, err := config.ResolveCompliance(p, s.files)
	return err
}

// desired is the policy the resolver asks for (the version is set by the
// caller). When the collector configuration does not resolve, the error is
// returned together with the traffic part of the policy, which stands on its
// own: a payload tap must be announced whatever happens to the collector.
func (s *ComplianceService) desired(res *config.Resolver) (compliance.Policy, config.ComplianceConfig, error) {
	cfg, cerr := config.ResolveCompliance(res, s.files)
	p := compliance.Policy{Level: compliance.LevelOff}
	lvl, err := res.Resolve(config.KeyTrafficLog)
	if err != nil {
		return compliance.Policy{}, cfg, err
	}
	if lvl.Value == config.TrafficPayload {
		p.TrafficPayload = true
		p.TrafficID = trafficID(res)
	}
	if cerr != nil {
		return p, cfg, cerr
	}
	p.Level, p.Input = cfg.Level, cfg.Input
	if cfg.Active() {
		p.Destination = cfg.Destination()
		p.DestID = digestOf(cfg.LogsURL)
	}
	return p, cfg, nil
}

func digestOf(parts ...string) string {
	h := sha256.New()
	for _, x := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s;", len(x), x)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// trafficID identifies where the payload log goes and for which Workers; a
// change of either is an effective change of the policy.
func trafficID(res *config.Resolver) string {
	to, _ := res.Resolve(config.KeyTrafficLogTo)
	ws, _ := res.Resolve(config.KeyTrafficLogWorkers)
	return digestOf(to.Value, ws.Value)
}

func policyOfRow(r store.CompliancePolicyRow) compliance.Policy {
	return compliance.Policy{Version: r.Version, Level: r.Level, Input: r.Input, TrafficPayload: r.TrafficPayload,
		Destination: r.DestLabel, DestID: r.DestID, TrafficID: r.TrafficID}
}

// syncTx writes the policy row and the audit rows of an effective change in
// the transaction of the settings change.
func (s *ComplianceService) syncTx(ctx context.Context, tx *sql.Tx, proposed *config.Resolver, actor SettingsActor, _ []string) error {
	want, _, err := s.desired(proposed)
	if err != nil {
		return fmt.Errorf("compliance: the policy of the change does not resolve: %w", err)
	}
	_, _, err = s.syncPolicy(ctx, tx, want, actor, "")
	return err
}

// syncPolicy compares want with the stored policy and, when they differ, writes
// the next version and the audit rows. startup, when not empty, names the
// provenance of a start ("environment" or "panel") and writes the startup row
// besides. It returns the policy in force after the write.
func (s *ComplianceService) syncPolicy(ctx context.Context, tx *sql.Tx, want compliance.Policy, actor SettingsActor, startup string) (compliance.Policy, bool, error) {
	row, have, err := store.GetCompliancePolicyTx(ctx, tx)
	if err != nil {
		return compliance.Policy{}, false, err
	}
	old := compliance.Policy{Level: compliance.LevelOff}
	if have {
		old = policyOfRow(row)
	}
	record := func(action string, p compliance.Policy) error {
		return s.aud.RecordSettingsEventTx(ctx, tx, SettingsAuditEvent{
			Actor: actor.ID, ActingAs: actor.ActingAs, Action: action, Target: p.AuditTarget(), Outcome: AuditOutcomeOK})
	}
	changed := false
	cur := old
	if !have && !want.Active() {
		cur = want
	} else if !old.Same(want) {
		changed = true
		cur = want
		cur.Version = old.Version + 1
		if err := store.PutCompliancePolicyTx(ctx, tx, store.CompliancePolicyRow{
			Version: cur.Version, Level: cur.Level, Input: cur.Input, TrafficPayload: cur.TrafficPayload,
			DestLabel: cur.Destination, DestID: cur.DestID, TrafficID: cur.TrafficID, UpdatedAt: store.UnixMS(s.now()), UpdatedBy: actor.ID,
		}); err != nil {
			return compliance.Policy{}, false, err
		}
		var actions []string
		switch {
		case !old.Active() && cur.Active():
			actions = []string{AuditComplianceEnable}
		case old.Active() && !cur.Active():
			actions = []string{AuditComplianceDisable}
		default:
			if old.Destination != cur.Destination || old.DestID != cur.DestID {
				actions = append(actions, AuditComplianceRetarget)
			}
			if old.Level != cur.Level || old.Input != cur.Input || old.TrafficPayload != cur.TrafficPayload || old.TrafficID != cur.TrafficID {
				actions = append(actions, AuditComplianceChange)
			}
		}
		for _, a := range actions {
			if err := record(a, cur); err != nil {
				return compliance.Policy{}, false, err
			}
		}
	} else {
		cur = old
	}
	if startup != "" {
		if err := record(AuditComplianceStartup, cur); err != nil {
			return compliance.Policy{}, false, err
		}
	}
	return cur, changed, nil
}

// start applies the configuration at the Hub's start: the policy row follows
// the environment and the panel, a startup row records the provenance when the
// environment sets any compliance key or monitoring is on, and the exporter is
// built. A collector configuration that does not resolve never hides the
// traffic payload level: that part of the policy is published on its own.
func (s *ComplianceService) start(ctx context.Context) error {
	res := s.settings.Resolver()
	want, cfg, err := s.desired(res)
	var cerr error
	if err != nil {
		cerr = err
		if _, terr := res.Resolve(config.KeyTrafficLog); terr != nil {
			// Not even the traffic level resolves: nothing is recorded, nothing is announced.
			s.fail(ctx, "start", err)
			return nil
		}
		cfg = config.ComplianceConfig{}
	}
	provenance := ""
	actor := SettingsActor{ID: "panel"}
	if cfg.Env {
		actor = SettingsActor{ID: "environment"}
	}
	if cfg.Env || want.Active() || cerr != nil {
		provenance = actor.ID
	}
	var cur compliance.Policy
	if err := s.store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		cur, _, err = s.syncPolicy(ctx, tx, want, actor, provenance)
		return err
	}); err != nil {
		return fmt.Errorf("compliance: write the policy: %w", err)
	}
	if cerr != nil {
		// Fail closed on the export, not on the Hub.
		_ = s.install(ctx, cur, config.ComplianceConfig{}, actor)
		s.fail(ctx, "start", cerr)
		return nil
	}
	// A failed export is already audited and logged by install; the Hub starts.
	_ = s.install(ctx, cur, cfg, actor)
	return nil
}

// applyTimeout bounds the work after a committed change. It runs on a context
// the Hub owns: a browser that went away right after the commit must not leave
// the old exporter running under a policy that was audited as changed.
const applyTimeout = 15 * time.Second

// apply runs after a committed change: it publishes the policy in force and
// rebuilds the exporter when the configuration changed, as one generation.
func (s *ComplianceService) apply(ctx context.Context, eff *config.Resolver) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()
	row, ok, err := s.store.GetCompliancePolicy(ctx)
	if err != nil {
		s.failClosed(err)
		return err
	}
	pol := compliance.Policy{Level: compliance.LevelOff}
	if ok {
		pol = policyOfRow(row)
	}
	cfg, cerr := config.ResolveCompliance(eff, s.files)
	actor := SettingsActor{ID: "panel"}
	if row.UpdatedBy != "" {
		actor.ID = row.UpdatedBy
	}
	if cerr != nil {
		_ = s.install(ctx, pol, config.ComplianceConfig{}, actor)
		s.fail(ctx, "apply", cerr)
		return cerr
	}
	return s.install(ctx, pol, cfg, actor)
}

// failClosed stops the export when the committed policy cannot be read: the
// policy in force stays (users are told), nothing more is sent anywhere.
func (s *ComplianceService) failClosed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = logx.ErrorText(err)
	s.Monitor.Install(s.Monitor.Policy(), nil)
	s.retire(s.exp)
	s.exp, s.cfgDig = nil, ""
	s.log.Error("compliance policy could not be applied; nothing is exported", "error", s.lastErr)
}

// retire discards the queue of an exporter that was replaced and keeps its
// counters for the diagnostics.
func (s *ComplianceService) retire(old ComplianceExporter) {
	if old == nil {
		return
	}
	old.Discard()
	_ = old.Close(expired())
	st := old.Status()
	if s.carry == nil {
		s.carry = map[string]uint64{}
	}
	for k, v := range st.Dropped {
		s.carry[k] += v
	}
	s.carrySent += st.Sent
}

// install makes pol and the exporter of cfg the generation in force. The new
// exporter is built and seeded with its policy record first; only then are the
// policy and the exporter published together (Monitor.Install), so no capture
// is admitted before its destination exists and none can reach a destination
// its policy did not name. The old exporter is retired after the swap; records
// of calls admitted under it are dropped and counted.
func (s *ComplianceService) install(ctx context.Context, pol compliance.Policy, cfg config.ComplianceConfig, actor SettingsActor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.exp
	cur := s.Monitor.Policy()
	if !cfg.Active() {
		if old == nil && cur.Version == pol.Version && cur.Same(pol) {
			s.cfgDig, s.lastErr = "", ""
			return nil
		}
		s.Monitor.Install(pol, nil)
		s.retire(old)
		s.exp, s.cfgDig, s.lastErr = nil, "", ""
		return nil
	}
	if old != nil && s.cfgDig == cfg.Digest {
		if cur.Version != pol.Version || !cur.Same(pol) {
			// Same destination, new policy: still one atomic swap, with a fresh dedupe.
			old.Emit(s.policyRecord(pol))
			s.Monitor.Install(pol, old)
		}
		return nil
	}
	s.gen++
	var next ComplianceExporter
	var err error
	if s.build == nil {
		err = errors.New("compliance export is not available in this build")
	} else {
		next, err = s.build(cfg, s.proc)
	}
	if err != nil {
		// Fail closed: nothing is exported; the policy still says monitored, so
		// users are told, and records are counted as not delivered.
		s.Monitor.Install(pol, nil)
		s.retire(old)
		s.exp, s.cfgDig = nil, ""
		s.lastErr = logx.ErrorText(err)
		s.auditOutside(ctx, actor, AuditComplianceFailed, pol)
		s.log.Error("compliance export could not be started; session records are not delivered", "error", s.lastErr)
		return err
	}
	next.Emit(s.policyRecord(pol))
	s.Monitor.Install(pol, next)
	s.retire(old)
	s.exp, s.cfgDig, s.lastErr = next, cfg.Digest, ""
	s.auditOutside(ctx, actor, AuditComplianceEffective, pol)
	return nil
}

// fail records an export that could not be configured.
func (s *ComplianceService) fail(ctx context.Context, stage string, err error) {
	s.mu.Lock()
	s.lastErr = logx.ErrorText(err)
	s.mu.Unlock()
	pol := s.Monitor.Policy()
	s.auditOutside(ctx, SettingsActor{ID: "environment"}, AuditComplianceFailed, pol)
	s.log.Error("compliance monitoring could not be configured; nothing is exported", "stage", stage, "error", s.lastErr)
}

// auditOutside writes an audit row in a transaction of its own (the exporter
// started or failed after the change committed).
func (s *ComplianceService) auditOutside(ctx context.Context, actor SettingsActor, action string, pol compliance.Policy) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settingsLookupTimeout)
	defer cancel()
	err := s.store.Tx(actx, func(tx *sql.Tx) error {
		return s.aud.RecordSettingsEventTx(actx, tx, SettingsAuditEvent{
			Actor: actor.ID, ActingAs: actor.ActingAs, Action: action, Target: pol.AuditTarget(), Outcome: AuditOutcomeOK})
	})
	if err != nil {
		s.log.Error("compliance audit row could not be written", "action", action, "error", logx.ErrorText(err))
	}
}

// policyRecord is the first record of a stream: the policy in force, so that
// the collector sees the activation.
func (s *ComplianceService) policyRecord(p compliance.Policy) compliance.Record {
	return compliance.Record{Event: compliance.EventPolicy, Time: s.now(), Attrs: []compliance.Attr{
		{Key: "minion.policy.version", Value: p.Version},
		{Key: "minion.policy.level", Value: p.Level},
		{Key: "minion.policy.input", Value: p.Input},
		{Key: "minion.policy.traffic_payload", Value: p.TrafficPayload},
		{Key: "minion.policy.destination", Value: p.Destination},
	}}
}

// Close flushes the exporter within ctx.
func (s *ComplianceService) Close(ctx context.Context) {
	s.mu.Lock()
	exp := s.exp
	s.exp = nil
	s.mu.Unlock()
	if exp != nil {
		s.Monitor.SetSink(nil)
		_ = exp.Close(ctx)
	}
}

// Status is the state of the compliance export for the diagnostics; nil when
// monitoring is off and nothing was ever dropped or sent. The counters of
// exporters that were replaced or stopped are carried over, so a retarget or a
// disable under load does not make its losses disappear.
func (s *ComplianceService) Status() *ops.LogSinkStatus {
	s.mu.Lock()
	exp, lastErr, gen := s.exp, s.lastErr, s.gen
	carry := map[string]uint64{}
	for k, v := range s.carry {
		carry[k] = v
	}
	carrySent := s.carrySent
	s.mu.Unlock()
	pol := s.Monitor.Policy()
	if !pol.Records() && len(carry) == 0 && carrySent == 0 && lastErr == "" {
		return nil
	}
	st := ops.LogSinkStatus{Name: "compliance", Enabled: pol.Records(), Destination: pol.Destination, Level: pol.Level, Generation: gen}
	if exp != nil {
		st = exp.Status()
		st.Name, st.Enabled, st.Destination, st.Level, st.Generation = "compliance", true, pol.Destination, pol.Level, gen
	}
	st.Sent += carrySent
	if st.Dropped == nil {
		st.Dropped = map[string]uint64{}
	}
	for k, v := range carry {
		st.Dropped[k] += v
	}
	if st.LastError == "" {
		st.LastError = lastErr
	}
	if n := s.Monitor.NoSink(); n > 0 {
		st.Dropped["no_exporter"] += n
	}
	return &st
}

// expired is an already-expired context: a discarded exporter gives up at once.
func expired() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// policyInfo converts a policy for the operations.
func policyInfo(p compliance.Policy, row *store.CompliancePolicyRow) ops.CompliancePolicy {
	i := ops.CompliancePolicy{Version: p.Version, Level: p.Level, Input: "off", Traffic: "metadata", Destination: p.Destination}
	if i.Level == "" {
		i.Level = compliance.LevelOff
	}
	if p.Input {
		i.Input = "on"
	}
	if p.TrafficPayload {
		i.Traffic = "payload"
	}
	if row != nil && row.UpdatedAt > 0 {
		t := ops.Timestamp(time.UnixMilli(row.UpdatedAt).UTC())
		i.UpdatedAt = &t
		i.UpdatedBy = row.UpdatedBy
	}
	return i
}

// Set is compliance_set: the caller is a fresh administrator browser session
// (the handler checked). The change is validated as a whole, stored in one
// transaction with the policy version and its audit rows, and applied.
func (s *ComplianceService) Set(ctx context.Context, actor SettingsActor, a ops.ComplianceSetArgs) (ops.ComplianceSetResult, error) {
	res, err := s.settings.SetGated(ctx, actor, ops.SettingsSetArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Changes: a.Changes, Expect: a.Expect,
	})
	if err != nil {
		return ops.ComplianceSetResult{}, err
	}
	out := ops.ComplianceSetResult{Warnings: res.Warnings}
	if out.Warnings == nil {
		out.Warnings = []ops.Warning{}
	}
	out.Policy = s.policyNow(ctx)
	return out, nil
}

func (s *ComplianceService) policyNow(ctx context.Context) ops.CompliancePolicy {
	row, ok, err := s.store.GetCompliancePolicy(ctx)
	if err != nil || !ok {
		return policyInfo(s.Monitor.Policy(), nil)
	}
	return policyInfo(policyOfRow(row), &row)
}

// registerComplianceOps binds compliance_set and compliance_status.
func registerComplianceOps(r *api.Registry, svc *ComplianceService) error {
	if err := api.HandleTyped(r, ops.OpComplianceSet, func(ctx context.Context, c *api.Call, a ops.ComplianceSetArgs) (ops.ComplianceSetResult, error) {
		if err := api.RequireFreshSession(c, svc.now()); err != nil {
			return ops.ComplianceSetResult{}, err
		}
		return svc.Set(ctx, SettingsActor{ID: c.Identity.Actor}, a)
	}); err != nil {
		return err
	}
	return api.HandleTyped(r, ops.OpComplianceStatus, func(ctx context.Context, c *api.Call, _ ops.ComplianceStatusArgs) (ops.ComplianceStatusResult, error) {
		if err := requireHubWideScope(c.Principal); err != nil {
			return ops.ComplianceStatusResult{}, err
		}
		return ops.ComplianceStatusResult{Policy: svc.policyNow(ctx), Export: svc.Status()}, nil
	})
}
