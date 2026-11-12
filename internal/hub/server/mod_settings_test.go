package server

import (
	"context"
	"errors"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type fakeAuditor struct {
	mu     sync.Mutex
	events []SettingsAuditEvent
	err    error
}

func (f *fakeAuditor) RecordSettingsEvent(_ context.Context, e SettingsAuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, e)
	return nil
}

func (f *fakeAuditor) all() []SettingsAuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SettingsAuditEvent(nil), f.events...)
}

const (
	keyGreeting = "greeting"    // plain, live
	keyMode     = "mode"        // plain, not live, with choices, default "fast"
	keyToken    = "api_token"   // secret, live
	keyVault    = "vault_token" // secret, not live
	keyWorker   = "worker_only" // worker scope
	keyBadDef   = "bad_default" // plain, not live, its default cannot be computed
	secretValue = "hunter2-very-secret"
)

func testRegistry() *config.Registry {
	r := config.NewRegistry()
	r.Declare(config.Setting{Key: keyGreeting, Scope: config.ScopeHub, Help: "Greeting", Default: "hello"})
	r.Declare(config.Setting{Key: keyMode, Scope: config.ScopeHub, Default: "fast", Choices: []string{"fast", "slow"}})
	r.Declare(config.Setting{Key: keyToken, Scope: config.ScopeHub, Secret: true})
	r.Declare(config.Setting{Key: keyVault, Scope: config.ScopeHub, Secret: true})
	r.Declare(config.Setting{Key: keyWorker, Scope: config.ScopeWorker})
	r.Declare(config.Setting{Key: config.KeyDataDir, Scope: config.ScopeAny, Default: "/default/data"})
	r.Declare(config.Setting{Key: keyBadDef, Scope: config.ScopeHub,
		DefaultFunc: func() (string, error) { return "", errors.New("no default here") }})
	return r
}

type env map[string]string

func (e env) lookup(name string) (string, bool) { v, ok := e[name]; return v, ok }

type fixture struct {
	t     *testing.T
	st    *store.Store
	aud   *fakeAuditor
	env   env
	files map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := hubstore.Open(t, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &fixture{t: t, st: st, aud: &fakeAuditor{}, env: env{}, files: map[string]string{}}
}

// service builds a service as at a Hub start: the snapshot of non-live
// settings is taken now.
func (f *fixture) service() *SettingsService {
	f.t.Helper()
	svc, err := NewSettingsService(context.Background(), SettingsDeps{
		Store: f.st, Auditor: f.aud, Registry: testRegistry(),
		Live:      func(k string) bool { return k == keyGreeting || k == keyToken },
		LookupEnv: f.env.lookup,
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := f.files[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("no such file")
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return svc
}

var admin = SettingsActor{ID: "admin-1"}

func setArgs(key, value string) ops.SettingsSetArgs {
	return ops.SettingsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: key, Value: value}
}

func listByKey(t *testing.T, svc *SettingsService) map[string]ops.SettingInfo {
	t.Helper()
	res, err := svc.List(context.Background(), ops.SettingsListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]ops.SettingInfo{}
	for _, s := range res.Settings {
		m[s.Key] = s
	}
	return m
}

func resolve(t *testing.T, svc *SettingsService, key string) config.Resolved {
	t.Helper()
	r, err := svc.Resolver().Resolve(key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// AC1: a Hub setting set by environment is listed read-only, and settings_set
// returns conflict "set by environment variable".
func TestEnvSettingIsReadOnlyAndSetConflicts(t *testing.T) {
	f := newFixture(t)
	f.env["MINION_GREETING"] = "from-env"
	svc := f.service()

	info := listByKey(t, svc)[keyGreeting]
	if info.Value != "from-env" || info.Source != "env" || !info.ReadOnly {
		t.Fatalf("list entry = %+v", info)
	}

	_, err := svc.Set(context.Background(), admin, setArgs(keyGreeting, "from-panel"))
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeConflict || pe.Message != "set by environment variable" {
		t.Fatalf("Set error = %v, want conflict %q", err, "set by environment variable")
	}
	if _, ok, _ := f.st.GetSetting(context.Background(), keyGreeting); ok {
		t.Fatal("a refused change was stored")
	}
	ev := f.aud.all()
	if len(ev) != 1 || ev[0].Outcome != AuditOutcomeRefused || ev[0].Target != keyGreeting {
		t.Fatalf("audit = %+v", ev)
	}
}

func TestEnvWinsOverAStoredPanelValue(t *testing.T) {
	f := newFixture(t)
	svc := f.service()
	if _, err := svc.Set(context.Background(), admin, setArgs(keyGreeting, "from-panel")); err != nil {
		t.Fatal(err)
	}
	if r := resolve(t, svc, keyGreeting); r.Value != "from-panel" || r.Source != config.SourcePanel {
		t.Fatalf("before env: %v", r)
	}
	f.env["MINION_GREETING"] = "from-env"
	r := resolve(t, svc, keyGreeting)
	if r.Value != "from-env" || r.Source != config.SourceEnv || !r.ReadOnly() {
		t.Fatalf("after env: %v", r)
	}
	if info := listByKey(t, svc)[keyGreeting]; info.Source != "env" || !info.ReadOnly || info.Value != "from-env" {
		t.Fatalf("list: %+v", info)
	}
}

func TestSecretFromEnvFileIsMaskedAndReadOnly(t *testing.T) {
	f := newFixture(t)
	f.env["MINION_API_TOKEN_FILE"] = "/run/secrets/token"
	f.files["/run/secrets/token"] = secretValue + "\n"
	svc := f.service()

	info := listByKey(t, svc)[keyToken]
	if info.Source != "env" || !info.ReadOnly || !info.Secret || info.Value != config.Redacted {
		t.Fatalf("list entry = %+v", info)
	}
	if info.EnvFile != "MINION_API_TOKEN_FILE" {
		t.Fatalf("EnvFile = %q", info.EnvFile)
	}
	_, err := svc.Set(context.Background(), admin, setArgs(keyToken, "other"))
	if proto.CodeOf(err) != proto.CodeConflict {
		t.Fatalf("Set error = %v", err)
	}
}

func TestPlainEnvForSecretIsAConflictNotALeak(t *testing.T) {
	f := newFixture(t)
	f.env["MINION_API_TOKEN"] = secretValue // not accepted for a secret
	svc := f.service()

	info := listByKey(t, svc)[keyToken]
	if info.Error == "" || strings.Contains(info.Error, secretValue) {
		t.Fatalf("list entry = %+v", info)
	}
	if info.Source != "env" || !info.ReadOnly {
		t.Fatalf("a plain env variable for a secret must show as env and read-only: %+v", info)
	}
	_, err := svc.Set(context.Background(), admin, setArgs(keyToken, "x"))
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeConflict || pe.Message != ReadOnlyMessage || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("Set error = %v", err)
	}
	if ev := f.aud.all(); len(ev) != 1 || ev[0].Outcome != AuditOutcomeRefused || ev[0].Target != keyToken {
		t.Fatalf("audit = %+v", ev)
	}
}

// R72: an environment variable with an invalid value still owns the setting.
func TestInvalidEnvChoiceIsStillSetByEnvironment(t *testing.T) {
	f := newFixture(t)
	f.env["MINION_MODE"] = "bogus"
	svc := f.service()

	info := listByKey(t, svc)[keyMode]
	if !info.ReadOnly || info.Source != "env" || info.Error == "" {
		t.Fatalf("list entry = %+v", info)
	}
	_, err := svc.Set(context.Background(), admin, setArgs(keyMode, "slow"))
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeConflict || pe.Message != "set by environment variable" {
		t.Fatalf("Set error = %v", err)
	}
	ev := f.aud.all()
	if len(ev) != 1 || ev[0].Outcome != AuditOutcomeRefused || ev[0].Target != keyMode {
		t.Fatalf("audit = %+v", ev)
	}
	if _, ok, _ := f.st.GetSetting(context.Background(), keyMode); ok {
		t.Fatal("a refused change was stored")
	}
}

// An empty value counts as unset: it neither overrides the default nor skips
// the choices check.
func TestEmptyValueIsAnUnset(t *testing.T) {
	for _, key := range []string{keyMode, keyGreeting} { // not live, live
		t.Run(key, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			svc := f.service()
			if _, err := svc.Set(ctx, admin, setArgs(key, map[string]string{keyMode: "slow", keyGreeting: "hi"}[key])); err != nil {
				t.Fatal(err)
			}
			res, err := svc.Set(ctx, admin, setArgs(key, ""))
			if err != nil {
				t.Fatalf("empty value: %v", err)
			}
			if (len(res.Warnings) == 1) == svc.live(key) {
				t.Fatalf("restart warning = %+v for live=%v", res.Warnings, svc.live(key))
			}
			if _, ok, _ := f.st.GetSetting(ctx, key); ok {
				t.Fatal("an empty value was stored")
			}
			ev := f.aud.all()
			if last := ev[len(ev)-1]; last.Action != AuditActionSettingUnset || last.Target != key {
				t.Fatalf("audit = %+v", ev)
			}
			svc2 := f.service() // after a restart
			want := map[string]string{keyMode: "fast", keyGreeting: "hello"}[key]
			if r := resolve(t, svc2, key); r.Value != want || r.Source != config.SourceDefault {
				t.Fatalf("resolved %v, want default %q", r, want)
			}
		})
	}
}

func TestStoredEmptyValueIsNotAPanelValue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, k := range []string{keyMode, keyGreeting} {
		if err := f.st.PutSetting(ctx, k, "", false, ""); err != nil {
			t.Fatal(err)
		}
	}
	svc := f.service()
	for k, want := range map[string]string{keyMode: "fast", keyGreeting: "hello"} {
		if r := resolve(t, svc, k); r.Value != want || r.Source != config.SourceDefault {
			t.Fatalf("%s resolved %v, want default %q", k, r, want)
		}
		if _, ok, _ := svc.Panel().Lookup(k); ok {
			t.Fatalf("%s: panel holds an empty value", k)
		}
	}
}

func TestDataDirCannotBeSetInThePanel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	svc := f.service()
	_, err := svc.Set(ctx, admin, setArgs(config.KeyDataDir, "/elsewhere"))
	var ae *ops.ArgumentError
	if !errors.As(err, &ae) || ae.Field != "key" || ae.Reason != "cannot be set in the panel; use MINION_DATA_DIR" {
		t.Fatalf("Set error = %v", err)
	}
	if len(f.aud.all()) != 0 {
		t.Fatalf("audit = %+v", f.aud.all())
	}
	// A row that got into the store anyway is never used.
	if err := f.st.PutSetting(ctx, config.KeyDataDir, "/elsewhere", false, ""); err != nil {
		t.Fatal(err)
	}
	svc = f.service()
	if _, ok, _ := svc.Panel().Lookup(config.KeyDataDir); ok {
		t.Fatal("the panel returned a data_dir")
	}
	info := listByKey(t, svc)[config.KeyDataDir]
	if !info.ReadOnly || info.Source != "default" || info.Value != "/default/data" || info.PendingRestart {
		t.Fatalf("list entry = %+v", info)
	}
	f.env["MINION_DATA_DIR"] = "/from/env"
	if info := listByKey(t, svc)[config.KeyDataDir]; !info.ReadOnly || info.Source != "env" || info.Value != "/from/env" {
		t.Fatalf("list entry with env = %+v", info)
	}
}

func TestFailingDefaultDoesNotBlockStoringAPanelValue(t *testing.T) {
	f := newFixture(t)
	svc := f.service()
	if info := listByKey(t, svc)[keyBadDef]; info.Error == "" || info.Source != "default" || info.ReadOnly {
		t.Fatalf("list entry = %+v", info)
	}
	if _, err := svc.Set(context.Background(), admin, setArgs(keyBadDef, "explicit")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	svc2 := f.service()
	if r := resolve(t, svc2, keyBadDef); r.Value != "explicit" || r.Source != config.SourcePanel {
		t.Fatalf("resolved %v", r)
	}
}

func TestInvalidStoredValueIsListedAsPanelWithPendingRepair(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.st.PutSetting(ctx, keyMode, "bogus", false, ""); err != nil {
		t.Fatal(err)
	}
	svc := f.service()
	info := listByKey(t, svc)[keyMode]
	if info.Error == "" || info.Source != "panel" || info.ReadOnly || info.PendingRestart {
		t.Fatalf("list entry = %+v", info)
	}
	if _, err := svc.Set(ctx, admin, setArgs(keyMode, "fast")); err != nil {
		t.Fatal(err)
	}
	// The invalid startup snapshot stays active until a restart.
	info = listByKey(t, svc)[keyMode]
	if info.Error == "" || !info.PendingRestart || info.Source != "panel" {
		t.Fatalf("after repair: %+v", info)
	}
	if info := listByKey(t, f.service())[keyMode]; info.Error != "" || info.PendingRestart || info.Value != "fast" {
		t.Fatalf("after restart: %+v", info)
	}
}

func TestUnreadableSecretFileKeepsEnvOwnership(t *testing.T) {
	f := newFixture(t)
	f.env["MINION_VAULT_TOKEN_FILE"] = "/missing"
	svc := f.service()
	info := listByKey(t, svc)[keyVault]
	if info.Error == "" || info.Source != "env" || !info.ReadOnly {
		t.Fatalf("list entry = %+v", info)
	}
}

// AC2: a value is persisted, secrets are encrypted, the change is audited
// with the key only.
func TestSetPersistsSecretEncryptedAndAuditsKeyOnly(t *testing.T) {
	f := newFixture(t)
	svc := f.service()
	ctx := context.Background()
	args := setArgs(keyToken, secretValue)

	res, err := svc.Set(ctx, SettingsActor{ID: "admin-1", ActingAs: "user-9"}, args)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ops.OutcomeDelivered || res.Validate() != nil {
		t.Fatalf("result = %+v", res)
	}

	var text *string
	var sealed []byte
	if err := f.st.DB().QueryRow(`SELECT value, value_enc FROM settings WHERE key = ?`, keyToken).Scan(&text, &sealed); err != nil {
		t.Fatal(err)
	}
	if text != nil || len(sealed) == 0 || strings.Contains(string(sealed), secretValue) {
		t.Fatalf("secret not encrypted: text=%v sealed=%x", text, sealed)
	}

	ev := f.aud.all()
	want := SettingsAuditEvent{Actor: "admin-1", ActingAs: "user-9", Action: "settings.set", Target: keyToken, Outcome: "ok"}
	if len(ev) != 1 || ev[0] != want {
		t.Fatalf("audit = %+v, want %+v", ev, want)
	}
	for _, e := range ev {
		if strings.Contains(strings.Join([]string{e.Actor, e.ActingAs, e.Action, e.Target, e.Outcome}, "|"), secretValue) {
			t.Fatal("audit event carries the value")
		}
	}

	// The secret is masked in the list and in every printable form.
	info := listByKey(t, svc)[keyToken]
	if info.Source != "panel" || info.ReadOnly || info.Value != config.Redacted {
		t.Fatalf("list entry = %+v", info)
	}
	r := resolve(t, svc, keyToken)
	if r.Value != secretValue || strings.Contains(r.String(), secretValue) || r.Display() != config.Redacted {
		t.Fatalf("resolved = %v", r)
	}
}

func TestLiveSettingIsEffectiveWithoutRestart(t *testing.T) {
	f := newFixture(t)
	svc := f.service() // started before the change
	if r := resolve(t, svc, keyGreeting); r.Value != "hello" || r.Source != config.SourceDefault {
		t.Fatalf("default: %v", r)
	}
	res, err := svc.Set(context.Background(), admin, setArgs(keyGreeting, "hi there"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("a live setting must not warn about a restart: %+v", res.Warnings)
	}
	if r := resolve(t, svc, keyGreeting); r.Value != "hi there" || r.Source != config.SourcePanel {
		t.Fatalf("after set: %v", r)
	}
	info := listByKey(t, svc)[keyGreeting]
	if !info.Live || info.PendingRestart || info.Value != "hi there" {
		t.Fatalf("list entry = %+v", info)
	}
	// Unset: the default applies again, at once.
	un := setArgs(keyGreeting, "")
	un.Unset = true
	if _, err := svc.Set(context.Background(), admin, un); err != nil {
		t.Fatal(err)
	}
	if r := resolve(t, svc, keyGreeting); r.Value != "hello" || r.Source != config.SourceDefault {
		t.Fatalf("after unset: %v", r)
	}
	ev := f.aud.all()
	if len(ev) != 2 || ev[1].Action != AuditActionSettingUnset || ev[1].Target != keyGreeting {
		t.Fatalf("audit = %+v", ev)
	}
}

func TestNonLiveSettingAppliesAfterRestart(t *testing.T) {
	f := newFixture(t)
	svc := f.service()
	res, err := svc.Set(context.Background(), admin, setArgs(keyMode, "slow"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != ops.WarnRestartRequired || res.Validate() != nil {
		t.Fatalf("warnings = %+v", res.Warnings)
	}
	// Not in effect yet, and the list says so.
	if r := resolve(t, svc, keyMode); r.Value != "fast" || r.Source != config.SourceDefault {
		t.Fatalf("before restart: %v", r)
	}
	info := listByKey(t, svc)[keyMode]
	if info.Live || !info.PendingRestart || info.Value != "fast" {
		t.Fatalf("list entry = %+v", info)
	}
	// A restart takes the new snapshot.
	svc2 := f.service()
	if r := resolve(t, svc2, keyMode); r.Value != "slow" || r.Source != config.SourcePanel {
		t.Fatalf("after restart: %v", r)
	}
	if info := listByKey(t, svc2)[keyMode]; info.PendingRestart {
		t.Fatalf("still pending after restart: %+v", info)
	}
	// Unsetting is pending too.
	un := setArgs(keyMode, "")
	un.Unset = true
	if _, err := svc2.Set(context.Background(), admin, un); err != nil {
		t.Fatal(err)
	}
	if info := listByKey(t, svc2)[keyMode]; !info.PendingRestart || info.Value != "slow" {
		t.Fatalf("unset not pending: %+v", info)
	}
}

func TestPanelSourcePrecedence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.st.PutSetting(ctx, keyMode, "slow", false, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutSetting(ctx, keyToken, secretValue, true, ""); err != nil {
		t.Fatal(err)
	}
	svc := f.service()
	p := svc.Panel()

	if v, ok, err := p.Lookup(keyMode); err != nil || !ok || v != "slow" {
		t.Fatalf("panel mode = %q %v %v", v, ok, err)
	}
	if v, ok, err := p.Lookup(keyToken); err != nil || !ok || v != secretValue {
		t.Fatalf("panel token = %v %v", ok, err)
	}
	// A key the panel has not set reports ok=false (not an empty value).
	for _, k := range []string{keyGreeting, keyVault, "unknown_key"} {
		if v, ok, err := p.Lookup(k); err != nil || ok || v != "" {
			t.Fatalf("Lookup(%s) = %q %v %v", k, v, ok, err)
		}
	}
	// default < panel < env
	if r := resolve(t, svc, keyMode); r.Value != "slow" || r.Source != config.SourcePanel {
		t.Fatalf("panel over default: %v", r)
	}
	f.env["MINION_MODE"] = "fast"
	if r := resolve(t, svc, keyMode); r.Value != "fast" || r.Source != config.SourceEnv {
		t.Fatalf("env over panel: %v", r)
	}
}

func TestListCoversHubSettingsOnly(t *testing.T) {
	f := newFixture(t)
	svc := f.service()
	m := listByKey(t, svc)
	if _, ok := m[keyWorker]; ok {
		t.Fatal("a Worker setting is listed")
	}
	if len(m) != 6 {
		t.Fatalf("listed %d settings, want 6", len(m))
	}
	g := m[keyGreeting]
	if g.Env != "MINION_GREETING" || g.Help != "Greeting" || g.Source != "default" || g.ReadOnly || g.Value != "hello" {
		t.Fatalf("greeting = %+v", g)
	}
	if mo := m[keyMode]; len(mo.Choices) != 2 {
		t.Fatalf("choices = %v", mo.Choices)
	}
	// Unset secret: nothing to mask.
	if v := m[keyVault]; v.Value != "" || v.Source != "default" {
		t.Fatalf("vault = %+v", v)
	}
}

func TestSetRejectsBadInput(t *testing.T) {
	f := newFixture(t)
	svc := f.service()
	ctx := context.Background()
	for name, args := range map[string]ops.SettingsSetArgs{
		"unknown key":    setArgs("no_such_key", "x"),
		"worker setting": setArgs(keyWorker, "x"),
		"bad choice":     setArgs(keyMode, "medium"),
		"no request id":  {Key: keyGreeting, Value: "x"},
	} {
		_, err := svc.Set(ctx, admin, args)
		if !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("%s: error = %v, want invalid_argument", name, err)
		}
	}
	if _, err := svc.Set(ctx, SettingsActor{}, setArgs(keyGreeting, "x")); err == nil {
		t.Error("a change without an actor was accepted")
	}
	if recs, _ := f.st.ListSettings(ctx); len(recs) != 0 {
		t.Errorf("rejected input was stored: %+v", recs)
	}
	if len(f.aud.all()) != 0 {
		t.Errorf("rejected input was audited as a change: %+v", f.aud.all())
	}
}

func TestInvalidStoredValueCanBeFixed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.st.PutSetting(ctx, keyMode, "bogus", false, ""); err != nil { // bypasses validation
		t.Fatal(err)
	}
	svc := f.service()
	if info := listByKey(t, svc)[keyMode]; info.Error == "" {
		t.Fatalf("an invalid stored value is not reported: %+v", info)
	}
	if _, err := svc.Set(ctx, admin, setArgs(keyMode, "fast")); err != nil {
		t.Fatalf("cannot fix an invalid stored value: %v", err)
	}
}

func TestAuditFailureIsReported(t *testing.T) {
	f := newFixture(t)
	svc := f.service()
	f.aud.err = errors.New("audit down")
	_, err := svc.Set(context.Background(), admin, setArgs(keyGreeting, "x"))
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("error = %v", err)
	}
	// The audit record comes first, so the failed change is not stored and a
	// live setting never took the unaudited value.
	if _, ok, _ := f.st.GetSetting(context.Background(), keyGreeting); ok {
		t.Fatal("a change whose audit record failed was stored")
	}
	if r := resolve(t, svc, keyGreeting); r.Source != config.SourceDefault {
		t.Fatalf("resolved %v", r)
	}
}

func TestNewSettingsServiceNeedsCollaborators(t *testing.T) {
	f := newFixture(t)
	if _, err := NewSettingsService(context.Background(), SettingsDeps{Auditor: f.aud}); err == nil {
		t.Error("no store accepted")
	}
	if _, err := NewSettingsService(context.Background(), SettingsDeps{Store: f.st}); err == nil {
		t.Error("no auditor accepted")
	}
}

func TestDeclareLiveSetting(t *testing.T) {
	if IsLiveSetting("declared_live_for_test") {
		t.Fatal("live before declaration")
	}
	DeclareLiveSetting("declared_live_for_test")
	if !IsLiveSetting("declared_live_for_test") {
		t.Fatal("not live after declaration")
	}
}

// The module provides both services once the store and an auditor exist.
func TestSettingsModuleWiring(t *testing.T) {
	f := newFixture(t)
	reg := NewRegistry()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(reg.Register("store", 10, func(_ context.Context, env *Env) error {
		return Provide(env, f.st)
	}, DeclaresService[*store.Store]()))
	must(reg.Register("audit", 11, func(_ context.Context, env *Env) error {
		return Provide[SettingsAuditor](env, f.aud)
	}, DeclaresService[SettingsAuditor]()))
	must(reg.Register(settingsModuleName, settingsModuleOrder, initSettingsModule,
		DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]()))

	k := NewKernel(reg)
	must(k.Start(context.Background()))
	defer func() { _ = k.Stop(context.Background()) }()
	if _, ok := Lookup[*SettingsService](k); !ok {
		t.Fatal("SettingsService not provided")
	}
	if _, ok := Lookup[config.PanelSource](k); !ok {
		t.Fatal("PanelSource not provided")
	}
}

func TestSettingsModuleFailsClearlyWithoutStore(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(settingsModuleName, settingsModuleOrder, initSettingsModule,
		DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]()); err != nil {
		t.Fatal(err)
	}
	err := NewKernel(reg).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), settingsModuleName) {
		t.Fatalf("error = %v", err)
	}
}

func TestSettingsModuleIsRegisteredInTheDefaultRegistry(t *testing.T) {
	if !slices.Contains(DefaultRegistry.Names(), settingsModuleName) {
		t.Fatalf("modules = %v", DefaultRegistry.Names())
	}
}
