package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
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

// SettingsTxAuditor is the transactional form of [SettingsAuditor]: the row
// is written in the caller's open transaction, so that a batch of changes and
// its audit rows commit together. An auditor without it records each row
// before its change, outside the transaction.
type SettingsTxAuditor interface {
	RecordSettingsEventTx(ctx context.Context, ex store.Execer, e SettingsAuditEvent) error
}

// settingsTxStore is the transactional form of [SettingsStore];
// *store.Store implements it.
type settingsTxStore interface {
	Tx(ctx context.Context, fn func(tx *sql.Tx) error) error
	PutSettingTx(ctx context.Context, ex store.Execer, key, value string, secret bool, updatedBy string) error
	DeleteSettingTx(ctx context.Context, ex store.Execer, key string) (bool, error)
}

// SettingsValidateFunc checks a proposed configuration before a change is
// stored: proposed resolves every Hub setting as it would be after the
// change (environment, then the panel with the change, then the default). An
// error refuses the whole change; its text must name keys, never values.
type SettingsValidateFunc func(proposed *config.Resolver) error

// SettingsApplyFunc applies the effective configuration after a change was
// stored. It runs on the goroutine of the change, never on a logging path.
// An error becomes the warning apply_failed of the change.
type SettingsApplyFunc func(ctx context.Context, effective *config.Resolver) error

// SettingsTxFunc runs inside the transaction of a change, after its settings
// rows were written and before it commits: proposed resolves every Hub setting
// as it is after the change, keys are the changed keys. An error rolls the
// whole change back (the compliance policy version and its audit rows commit
// with the settings, A49 T15).
type SettingsTxFunc func(ctx context.Context, tx *sql.Tx, proposed *config.Resolver, actor SettingsActor, keys []string) error

type settingsHook struct {
	prefixes []string
	validate SettingsValidateFunc
	apply    SettingsApplyFunc
	tx       SettingsTxFunc
}

func (h settingsHook) matches(key string) bool {
	for _, p := range h.prefixes {
		if key == p || strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
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

	// mu serialises changes: validation, commit and apply of one change
	// complete before the next change starts.
	mu    sync.Mutex
	hooks []settingsHook
}

// OnChange registers a hook for the settings whose key starts with one of
// prefixes (an exact key is a prefix of itself). validate (may be nil) runs
// before a change of such a key is stored, against the proposed view of all
// settings; apply (may be nil) runs after it was stored. Hooks run in
// registration order. Register hooks during module init.
func (s *SettingsService) OnChange(prefixes []string, validate SettingsValidateFunc, apply SettingsApplyFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = append(s.hooks, settingsHook{prefixes: slices.Clone(prefixes), validate: validate, apply: apply})
}

// OnChangeTx registers tx, which runs in the transaction of a change of any
// setting whose key starts with one of prefixes.
func (s *SettingsService) OnChangeTx(prefixes []string, tx SettingsTxFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = append(s.hooks, settingsHook{prefixes: slices.Clone(prefixes), tx: tx})
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
		info.Section, _ = config.SectionOf(st.Key)
		info.Revision = s.revision(ctx, st.Key)
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
		if config.SecretPathValue(st.Key, res.Value) {
			// A key pasted where its path belongs is never shown back.
			info.Value = config.Redacted
			info.Error = "the value is not a file path (it looks like the key itself); set the path of the key file"
		}
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

// Set stores or removes panel values: one setting (Key) or a batch
// (Changes), all or nothing. An empty value counts as unset, as an empty
// environment variable does. A setting that the environment sets is refused
// with a conflict whose message is "set by environment variable". A secret is
// encrypted at rest. Before anything is stored, the validators of the hooks
// ([SettingsService.OnChange]) see the proposed view and can refuse the
// change. Every change is audited with the key only: in the transaction of
// the change when the auditor supports it ([SettingsTxAuditor]), otherwise
// before it; when an audit record cannot be written, nothing is changed.
// After the commit the apply hooks run.
func (s *SettingsService) Set(ctx context.Context, actor SettingsActor, args ops.SettingsSetArgs) (ops.WriteResult, error) {
	return s.set(ctx, actor, args, false)
}

// GatedRefusal is the message of settings_set for a compliance setting.
const GatedRefusal = "compliance monitoring settings and the payload traffic level change only through compliance_set (a fresh sign-in of an administrator in the browser)"

// SetGated is Set for the compliance_set operation: it accepts the gated keys
// (and only them). The caller has checked who may change them.
func (s *SettingsService) SetGated(ctx context.Context, actor SettingsActor, args ops.SettingsSetArgs) (ops.WriteResult, error) {
	return s.set(ctx, actor, args, true)
}

func (s *SettingsService) set(ctx context.Context, actor SettingsActor, args ops.SettingsSetArgs, gateOpen bool) (ops.WriteResult, error) {
	if actor.ID == "" {
		return ops.WriteResult{}, errors.New("server: settings change without an actor")
	}
	if err := args.Validate(); err != nil {
		return ops.WriteResult{}, err
	}
	batch := len(args.Changes) > 0
	field := func(i int, name string) string {
		if batch {
			return fmt.Sprintf("changes[%d].%s", i, name)
		}
		return name
	}
	type change struct {
		st    config.Setting
		value string
		unset bool
	}
	var changes []change
	for i, c := range args.List() {
		st, ok := s.registry.Lookup(c.Key)
		if !ok || (st.Scope != config.ScopeHub && st.Scope != config.ScopeAny) {
			return ops.WriteResult{}, &ops.ArgumentError{Field: field(i, "key"), Reason: "unknown Hub setting"}
		}
		if st.Key == config.KeyDataDir {
			return ops.WriteResult{}, &ops.ArgumentError{Field: field(i, "key"), Reason: "cannot be set in the panel; use MINION_DATA_DIR"}
		}
		unset := c.Unset || c.Value == ""
		// The gate: the compliance keys and the payload traffic level change
		// only through compliance_set; compliance_set changes nothing else.
		// It comes before every other check of the value, so that a refusal
		// says nothing about what would have been accepted.
		gated := config.IsComplianceKey(st.Key)
		payload := st.Key == config.KeyTrafficLog && !unset && c.Value == config.TrafficPayload
		if payload && !gateOpen && s.storedValue(ctx, st.Key) == config.TrafficPayload {
			payload = false // the value does not change: a form that sends the section back as it was
		}
		if !gateOpen && (gated || payload) {
			_ = s.auditor.RecordSettingsEvent(ctx, SettingsAuditEvent{Actor: actor.ID, ActingAs: actor.ActingAs,
				Action: AuditActionSettingSet, Target: st.Key, Outcome: AuditOutcomeRefused})
			return ops.WriteResult{}, &ops.ArgumentError{Field: field(i, "key"), Reason: GatedRefusal}
		}
		if gateOpen && !gated && st.Key != config.KeyTrafficLog {
			return ops.WriteResult{}, &ops.ArgumentError{Field: field(i, "key"), Reason: "compliance_set changes compliance monitoring settings only"}
		}
		if !unset && len(st.Choices) > 0 && !slices.Contains(st.Choices, c.Value) {
			return ops.WriteResult{}, &ops.ArgumentError{Field: field(i, "value"), Reason: fmt.Sprintf("must be one of %v", st.Choices)}
		}
		changes = append(changes, change{st: st, value: c.Value, unset: unset})
	}
	eventOf := func(c change, outcome string) SettingsAuditEvent {
		action := AuditActionSettingSet
		if c.unset {
			action = AuditActionSettingUnset
		}
		return SettingsAuditEvent{Actor: actor.ID, ActingAs: actor.ActingAs, Action: action, Target: c.st.Key, Outcome: outcome}
	}

	// Whether the environment sets a setting is decided from the variables,
	// not from a resolution, so that neither an invalid environment value nor
	// an invalid stored value changes the answer.
	refused := false
	for _, c := range changes {
		if s.envSet(c.st) {
			refused = true
			if err := s.auditor.RecordSettingsEvent(ctx, eventOf(c, AuditOutcomeRefused)); err != nil {
				return ops.WriteResult{}, fmt.Errorf("server: audit refused settings change: %w", err)
			}
		}
	}
	if refused {
		return ops.WriteResult{}, proto.NewError(proto.CodeConflict, ReadOnlyMessage)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i, c := range args.List() {
		if c.Revision != "" && c.Revision != s.revision(ctx, c.Key) {
			return ops.WriteResult{}, proto.NewError(proto.CodeConflict,
				fmt.Sprintf("the setting %s was changed since this page was loaded; reload the page and save again", changes[i].st.Key))
		}
	}
	for _, e := range args.Expect {
		if _, ok := s.registry.Lookup(e.Key); !ok {
			return ops.WriteResult{}, &ops.ArgumentError{Field: "expect", Reason: "unknown Hub setting"}
		}
		if e.Revision != s.revision(ctx, e.Key) {
			return ops.WriteResult{}, proto.NewError(proto.CodeConflict,
				fmt.Sprintf("the setting %s was changed since this page was loaded; reload the page and save again", e.Key))
		}
	}

	keys := make([]string, len(changes))
	for i, c := range changes {
		keys[i] = c.st.Key
	}
	hooks := s.hooksFor(keys)
	var proposed *config.Resolver
	if len(hooks) > 0 {
		overlay := map[string]*string{}
		for _, c := range changes {
			v := c.value
			if c.unset {
				v = ""
			}
			overlay[c.st.Key] = &v
		}
		proposed = s.Resolver()
		proposed.Panel = overlayPanel{base: s.panel, over: overlay}
		for _, h := range hooks {
			if h.validate == nil {
				continue
			}
			if err := h.validate(proposed); err != nil {
				f := "value"
				if batch {
					f = "changes"
				}
				return ops.WriteResult{}, &ops.ArgumentError{Field: f, Reason: err.Error()}
			}
		}
	}

	txAud, audOK := s.auditor.(SettingsTxAuditor)
	txStore, stOK := s.store.(settingsTxStore)
	for _, h := range hooks {
		if h.tx != nil && (!audOK || !stOK) {
			return ops.WriteResult{}, errors.New("server: this setting changes only in one transaction with its audit rows, and the store cannot do that")
		}
	}
	if audOK && stOK {
		err := txStore.Tx(ctx, func(tx *sql.Tx) error {
			for _, c := range changes {
				if err := txAud.RecordSettingsEventTx(ctx, tx, eventOf(c, AuditOutcomeOK)); err != nil {
					return fmt.Errorf("server: setting %q was not changed, its audit record failed: %w", c.st.Key, err)
				}
				var err error
				if c.unset {
					_, err = txStore.DeleteSettingTx(ctx, tx, c.st.Key)
				} else {
					err = txStore.PutSettingTx(ctx, tx, c.st.Key, c.value, c.st.Secret, actor.ID)
				}
				if err != nil {
					return fmt.Errorf("server: store setting %q: %w", c.st.Key, err)
				}
			}
			for _, h := range hooks {
				if h.tx == nil {
					continue
				}
				if err := h.tx(ctx, tx, proposed, actor, keys); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return ops.WriteResult{}, err
		}
	} else {
		for _, c := range changes {
			if err := s.storeOne(ctx, actor, c.st, c.value, c.unset, eventOf(c, AuditOutcomeOK)); err != nil {
				return ops.WriteResult{}, err
			}
		}
	}

	res := ops.WriteResult{Outcome: ops.OutcomeDelivered}
	for _, c := range changes {
		if !s.live(c.st.Key) {
			res.Warnings = []ops.Warning{{Code: ops.WarnRestartRequired, Message: "takes effect after the Hub restarts"}}
			break
		}
	}
	for _, h := range hooks {
		if h.apply == nil {
			continue
		}
		if err := h.apply(ctx, s.Resolver()); err != nil {
			res.Warnings = append(res.Warnings, ops.Warning{Code: ops.WarnApplyFailed, Message: err.Error()})
		}
	}
	return res, nil
}

// storedValue is the panel value of key ("" when the panel holds none).
func (s *SettingsService) storedValue(ctx context.Context, key string) string {
	rec, ok, err := s.store.GetSetting(ctx, key)
	if err != nil || !ok {
		return ""
	}
	return rec.Value
}

// revision identifies the stored panel value of key: a hash of the key, the
// value and its change time under a salt of this process (a revision does
// not reveal a secret, and one from before a restart is stale). "none" when
// the panel holds no value.
func (s *SettingsService) revision(ctx context.Context, key string) string {
	rec, ok, err := s.store.GetSetting(ctx, key)
	if err != nil {
		return "unknown"
	}
	if !ok {
		return "none"
	}
	h := sha256.New()
	h.Write(revisionSalt[:])
	_, _ = fmt.Fprintf(h, "%d:%s|%d|%d:%s", len(key), key, rec.UpdatedAt.UnixNano(), len(rec.Value), rec.Value)
	return hex.EncodeToString(h.Sum(nil))[:24]
}

var revisionSalt = func() (b [32]byte) {
	_, _ = rand.Read(b[:])
	return b
}()

// storeOne is the change of one setting without a transaction: the audit
// record is written before the change, so that no change can exist without
// its record, and a live setting never takes an unaudited value, not even for
// a moment. When the change then fails, a second record with the outcome
// "failed" says so.
func (s *SettingsService) storeOne(ctx context.Context, actor SettingsActor, st config.Setting, value string, unset bool, event SettingsAuditEvent) error {
	if err := s.auditor.RecordSettingsEvent(ctx, event); err != nil {
		return fmt.Errorf("server: setting %q was not changed, its audit record failed: %w", st.Key, err)
	}
	var err error
	if unset {
		_, err = s.store.DeleteSetting(ctx, st.Key)
	} else {
		err = s.store.PutSetting(ctx, st.Key, value, st.Secret, actor.ID)
	}
	if err != nil {
		event.Outcome = AuditOutcomeFailed
		// Detached: the request context may be the reason the change failed.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settingsLookupTimeout)
		defer cancel()
		_ = s.auditor.RecordSettingsEvent(actx, event)
		return fmt.Errorf("server: store setting %q: %w", st.Key, err)
	}
	return nil
}

// hooksFor returns the hooks that match one of keys, in registration order.
func (s *SettingsService) hooksFor(keys []string) []settingsHook {
	var out []settingsHook
	for _, h := range s.hooks {
		for _, k := range keys {
			if h.matches(k) {
				out = append(out, h)
				break
			}
		}
	}
	return out
}

// overlayPanel is the panel as it would be after a change: the changed keys
// from over (an empty value is unset), every other key from base.
type overlayPanel struct {
	base config.PanelSource
	over map[string]*string
}

func (o overlayPanel) Lookup(key string) (string, bool, error) {
	if v, ok := o.over[key]; ok {
		if *v == "" || key == config.KeyDataDir {
			return "", false, nil
		}
		return *v, true, nil
	}
	return o.base.Lookup(key)
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
