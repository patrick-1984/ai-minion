package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// settingsModuleName and settingsModuleOrder place the module after the store
// (which it requires) and before every module that resolves Hub settings
// through the panel.
const (
	settingsModuleName  = "settings"
	settingsModuleOrder = 20
)

// ReadOnlyMessage is the message of the conflict returned when a setting is
// set by the environment (R72).
const ReadOnlyMessage = "set by environment variable"

// Audit vocabulary of settings changes (AD-20). The target is the setting key;
// the value is never part of an audit event.
const (
	AuditActionSettingSet   = "settings.set"
	AuditActionSettingUnset = "settings.unset"
	AuditOutcomeOK          = "ok"
	AuditOutcomeRefused     = "refused"
	AuditOutcomeFailed      = "failed"
)

// settingsLookupTimeout bounds one PanelSource lookup, which has no context.
const settingsLookupTimeout = 5 * time.Second

func init() {
	RegisterModule(settingsModuleName, settingsModuleOrder, initSettingsModule,
		DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]())
}

// SettingsStore is the persistence port of the panel settings layer;
// *store.Store implements it.
type SettingsStore interface {
	GetSetting(ctx context.Context, key string) (store.SettingRecord, bool, error)
	ListSettings(ctx context.Context) ([]store.SettingRecord, error)
	PutSetting(ctx context.Context, key, value string, secret bool, updatedBy string) error
	DeleteSetting(ctx context.Context, key string) (bool, error)
}

var _ SettingsStore = (*store.Store)(nil)

// SettingsAuditEvent is one audit record of a settings change: who, what,
// which key and the outcome, never a value.
type SettingsAuditEvent struct {
	Actor    string
	ActingAs string
	Action   string
	Target   string
	Outcome  string
}

// SettingsAuditor records settings changes. The audit module (Story 3.3)
// provides it; this layer only depends on the port. A recording failure is
// returned to the caller, so a change is never silent (AD-20).
type SettingsAuditor interface {
	RecordSettingsEvent(ctx context.Context, e SettingsAuditEvent) error
}

// SettingsActor identifies who changes a setting, for the audit record.
type SettingsActor struct {
	// ID is the acting account (or API key owner). Required.
	ID string
	// ActingAs is the account the actor acts as (admin act-as); empty when
	// none.
	ActingAs string
}

// liveSettings holds the keys whose panel value applies without a restart.
// config.Setting has no "live" field yet; the owner of a live setting
// declares it here from an init function until the registry gains one.
var liveSettings = struct {
	mu   sync.RWMutex
	keys map[string]struct{}
}{keys: map[string]struct{}{}}

// DeclareLiveSetting marks key as live (live=true, Story 3.4): a value stored
// in the panel takes effect without a Hub restart. The consumer must resolve
// the setting at the point of use instead of caching it.
func DeclareLiveSetting(key string) {
	liveSettings.mu.Lock()
	defer liveSettings.mu.Unlock()
	liveSettings.keys[key] = struct{}{}
}

// IsLiveSetting reports whether key was declared with [DeclareLiveSetting].
func IsLiveSetting(key string) bool {
	liveSettings.mu.RLock()
	defer liveSettings.mu.RUnlock()
	_, ok := liveSettings.keys[key]
	return ok
}

// SettingsDeps are the collaborators of a [SettingsService].
type SettingsDeps struct {
	Store   SettingsStore
	Auditor SettingsAuditor
	// Registry defaults to config.Default.
	Registry *config.Registry
	// Live defaults to [IsLiveSetting].
	Live func(key string) bool
	// LookupEnv and ReadFile default to the process environment and a
	// size-capped file read (see config.Resolver). Tests replace them.
	LookupEnv func(name string) (string, bool)
	ReadFile  func(path string) ([]byte, error)
}

// SettingsService implements the panel settings layer: the config.PanelSource
// of the Hub resolver, and the logic of the settings_list and settings_set
// operations. The REST framework (Story 3.9) calls List and Set after it has
// authorised the manage action.
type SettingsService struct {
	store    SettingsStore
	auditor  SettingsAuditor
	registry *config.Registry
	live     func(string) bool
	panel    *panelSource
	resolver config.Resolver // with the panel
	lookup   func(name string) (string, bool)
}

// NewSettingsService loads the panel values (the snapshot that non-live
// settings keep until the next restart) and returns the service.
func NewSettingsService(ctx context.Context, d SettingsDeps) (*SettingsService, error) {
	if d.Store == nil {
		return nil, errors.New("server: settings service needs a store")
	}
	if d.Auditor == nil {
		return nil, errors.New("server: settings service needs an auditor")
	}
	if d.Registry == nil {
		d.Registry = config.Default
	}
	if d.Live == nil {
		d.Live = IsLiveSetting
	}
	panel, err := newPanelSource(ctx, d.Store, d.Live)
	if err != nil {
		return nil, err
	}
	if d.LookupEnv == nil {
		d.LookupEnv = os.LookupEnv
	}
	resolver := config.Resolver{
		Registry: d.Registry, Role: config.ScopeHub, Panel: panel,
		LookupEnv: d.LookupEnv, ReadFile: d.ReadFile,
	}
	return &SettingsService{
		store: d.Store, auditor: d.Auditor, registry: d.Registry, live: d.Live,
		panel: panel, resolver: resolver, lookup: d.LookupEnv,
	}, nil
}

// Panel returns the config.PanelSource to hand to the Hub's resolver: the
// panel value of a live setting is read on every lookup, the value of any
// other setting is the one loaded at startup.
func (s *SettingsService) Panel() config.PanelSource { return s.panel }

// Resolver returns a Hub resolver over the service's registry and panel, with
// the environment of the process.
func (s *SettingsService) Resolver() *config.Resolver {
	r := s.resolver
	return &r
}

// List returns every Hub setting with its effective value (a secret is
// masked), its source and whether the panel may change it (R71, R72).
func (s *SettingsService) List(ctx context.Context, _ ops.SettingsListArgs) (ops.SettingsListResult, error) {
	pending, err := s.panel.pendingRestart(ctx)
	if err != nil {
		return ops.SettingsListResult{}, err
	}
	out := ops.SettingsListResult{Settings: []ops.SettingInfo{}}
	for _, st := range s.registry.ForScope(config.ScopeHub) {
		info := ops.SettingInfo{
			Key: st.Key, Help: st.Help, Env: st.EnvName(), Secret: st.Secret,
			Live: s.live(st.Key), Choices: slices.Clone(st.Choices),
		}
		if st.Secret {
			info.EnvFile = st.FileEnvName()
		}
		envSet := s.envSet(st)
		// data_dir is read before the panel store exists (it holds the
		// store), so the panel can never change it.
		info.ReadOnly = envSet || st.Key == config.KeyDataDir
		info.PendingRestart = pending[st.Key]
		res, err := s.resolver.Resolve(st.Key)
		if err != nil {
			// The source, the ownership and the pending state do not depend
			// on whether the value resolves.
			info.Error = err.Error()
			switch {
			case envSet:
				info.Source = string(config.SourceEnv)
			case s.panelHolds(st.Key):
				info.Source = string(config.SourcePanel)
			default:
				info.Source = ops.SettingSourceDefault
			}
			out.Settings = append(out.Settings, info)
			continue
		}
		info.Value = res.Display()
		info.Source = string(res.Source)
		info.ReadOnly = info.ReadOnly || res.ReadOnly()
		out.Settings = append(out.Settings, info)
	}
	return out, nil
}

// envSet reports whether the environment sets st: its variable is non-empty
// (an empty variable counts as unset, as in config.Resolver) or, for a secret,
// its "_FILE" variable is. It looks at the variables only, so an invalid
// value still counts as set by the environment (R72).
func (s *SettingsService) envSet(st config.Setting) bool {
	if v, ok := s.lookup(st.EnvName()); ok && v != "" {
		return true
	}
	if st.Secret {
		if v, ok := s.lookup(st.FileEnvName()); ok && v != "" {
			return true
		}
	}
	return false
}

// panelHolds reports whether the panel currently holds a value for key.
func (s *SettingsService) panelHolds(key string) bool {
	_, ok, err := s.panel.Lookup(key)
	return err == nil && ok
}

// Set stores or removes the panel value of a setting. An empty value counts
// as unset, as an empty environment variable does. A setting that the
// environment sets is refused with a conflict whose message is
// "set by environment variable". A secret is encrypted at rest. The change is
// audited with the key only, before it is made; when the audit record cannot
// be written, nothing is changed.
func (s *SettingsService) Set(ctx context.Context, actor SettingsActor, args ops.SettingsSetArgs) (ops.WriteResult, error) {
	if actor.ID == "" {
		return ops.WriteResult{}, errors.New("server: settings change without an actor")
	}
	if err := args.Validate(); err != nil {
		return ops.WriteResult{}, err
	}
	st, ok := s.registry.Lookup(args.Key)
	if !ok || (st.Scope != config.ScopeHub && st.Scope != config.ScopeAny) {
		return ops.WriteResult{}, &ops.ArgumentError{Field: "key", Reason: "unknown Hub setting"}
	}
	if st.Key == config.KeyDataDir {
		return ops.WriteResult{}, &ops.ArgumentError{Field: "key", Reason: "cannot be set in the panel; use MINION_DATA_DIR"}
	}
	unset := args.Unset || args.Value == ""
	if !unset && len(st.Choices) > 0 && !slices.Contains(st.Choices, args.Value) {
		return ops.WriteResult{}, &ops.ArgumentError{Field: "value", Reason: fmt.Sprintf("must be one of %v", st.Choices)}
	}

	action := AuditActionSettingSet
	if unset {
		action = AuditActionSettingUnset
	}
	event := SettingsAuditEvent{Actor: actor.ID, ActingAs: actor.ActingAs, Action: action, Target: st.Key}

	// Whether the environment sets the setting is decided from the variables,
	// not from a resolution, so that neither an invalid environment value nor
	// an invalid stored value changes the answer.
	if s.envSet(st) {
		event.Outcome = AuditOutcomeRefused
		if err := s.auditor.RecordSettingsEvent(ctx, event); err != nil {
			return ops.WriteResult{}, fmt.Errorf("server: audit refused settings change: %w", err)
		}
		return ops.WriteResult{}, proto.NewError(proto.CodeConflict, ReadOnlyMessage)
	}

	// The audit record is written before the change, so that no change can
	// exist without its record, and a live setting never takes an unaudited
	// value, not even for a moment. When the change then fails, a second
	// record with the outcome "failed" says so.
	event.Outcome = AuditOutcomeOK
	if err := s.auditor.RecordSettingsEvent(ctx, event); err != nil {
		return ops.WriteResult{}, fmt.Errorf("server: setting %q was not changed, its audit record failed: %w", st.Key, err)
	}
	var err error
	if unset {
		_, err = s.store.DeleteSetting(ctx, st.Key)
	} else {
		err = s.store.PutSetting(ctx, st.Key, args.Value, st.Secret, actor.ID)
	}
	if err != nil {
		event.Outcome = AuditOutcomeFailed
		// Detached: the request context may be the reason the change failed.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settingsLookupTimeout)
		defer cancel()
		_ = s.auditor.RecordSettingsEvent(actx, event)
		return ops.WriteResult{}, fmt.Errorf("server: store setting %q: %w", st.Key, err)
	}

	res := ops.WriteResult{Outcome: ops.OutcomeDelivered}
	if !s.live(st.Key) {
		res.Warnings = []ops.Warning{{Code: ops.WarnRestartRequired, Message: "takes effect after the Hub restarts"}}
	}
	return res, nil
}

// panelSource implements config.PanelSource on the settings store. Live
// settings read the store on every lookup. Every other setting returns the
// value that was stored when the Hub started, so that a change made in the
// panel applies at the next start (the list reports pending_restart).
type panelSource struct {
	store SettingsStore
	live  func(string) bool

	snapshot map[string]string // key -> value at startup, non-live keys only
}

func newPanelSource(ctx context.Context, st SettingsStore, live func(string) bool) (*panelSource, error) {
	recs, err := st.ListSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("server: load panel settings: %w", err)
	}
	p := &panelSource{store: st, live: live, snapshot: map[string]string{}}
	for _, r := range recs {
		if !live(r.Key) && panelCanHold(r) {
			p.snapshot[r.Key] = r.Value
		}
	}
	return p, nil
}

// panelCanHold reports whether a stored row is a panel value: an empty value
// counts as unset, and data_dir is never taken from the panel, because the
// panel table lives inside the data directory.
func panelCanHold(r store.SettingRecord) bool {
	return r.Value != "" && r.Key != config.KeyDataDir
}

// Lookup implements config.ValueSource.
func (p *panelSource) Lookup(key string) (string, bool, error) {
	if key == config.KeyDataDir {
		return "", false, nil
	}
	if !p.live(key) {
		v, ok := p.snapshot[key]
		return v, ok, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), settingsLookupTimeout)
	defer cancel()
	rec, ok, err := p.store.GetSetting(ctx, key)
	if err != nil || !ok || !panelCanHold(rec) {
		return "", false, err
	}
	return rec.Value, true, nil
}

// pendingRestart returns the non-live keys whose stored panel value differs
// from the one in effect.
func (p *panelSource) pendingRestart(ctx context.Context) (map[string]bool, error) {
	recs, err := p.store.ListSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("server: list panel settings: %w", err)
	}
	stored := map[string]string{}
	for _, r := range recs {
		if !p.live(r.Key) && panelCanHold(r) {
			stored[r.Key] = r.Value
		}
	}
	pending := map[string]bool{}
	for k, v := range stored {
		if sv, ok := p.snapshot[k]; !ok || sv != v {
			pending[k] = true
		}
	}
	for k := range p.snapshot {
		if _, ok := stored[k]; !ok {
			pending[k] = true
		}
	}
	return pending, nil
}

func initSettingsModule(ctx context.Context, env *Env) error {
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	aud, err := Require[SettingsAuditor](env)
	if err != nil {
		return err
	}
	svc, err := NewSettingsService(ctx, SettingsDeps{Store: st, Auditor: aud})
	if err != nil {
		return err
	}
	if err := Provide(env, svc); err != nil {
		return err
	}
	return Provide[config.PanelSource](env, svc.Panel())
}
