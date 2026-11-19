package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/obs"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

type fakeExporter struct {
	mu        sync.Mutex
	cfg       config.ComplianceConfig
	records   []compliance.Record
	discarded bool
	closed    bool
}

func (f *fakeExporter) Emit(r compliance.Record) {
	f.mu.Lock()
	f.records = append(f.records, r)
	f.mu.Unlock()
}
func (f *fakeExporter) Status() ops.LogSinkStatus { return ops.LogSinkStatus{Name: "compliance"} }
func (f *fakeExporter) Close(context.Context) error {
	f.closed = true
	return nil
}
func (f *fakeExporter) Discard() { f.discarded = true }

// failingAuditor fails the audit row of one action, in the transaction.
type failingAuditor struct {
	settingsAuditAdapter
	action string
}

func (a failingAuditor) RecordSettingsEventTx(ctx context.Context, ex store.Execer, e SettingsAuditEvent) error {
	if e.Action == a.action {
		return errors.New("audit refused")
	}
	return a.settingsAuditAdapter.RecordSettingsEventTx(ctx, ex, e)
}

type complianceFixture struct {
	*fixture
	ss    *SettingsService
	svc   *ComplianceService
	built []*fakeExporter
}

func newComplianceFixture(t *testing.T, mutate func(*fixture, *ComplianceService)) *complianceFixture {
	t.Helper()
	f := newFixture(t)
	ss, err := NewSettingsService(context.Background(), SettingsDeps{
		Store: f.st, Auditor: settingsAuditAdapter{log: audit.New(f.st)}, Registry: config.Default,
		Live: IsLiveSetting, LookupEnv: f.env.lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	cf := &complianceFixture{fixture: f, ss: ss}
	cf.svc = &ComplianceService{
		Monitor: compliance.NewMonitor(compliance.Deps{}), settings: ss, store: f.st,
		aud: settingsAuditAdapter{log: audit.New(f.st)}, log: slog.Default(), now: time.Now,
		build: func(c config.ComplianceConfig, _ obs.Process) (ComplianceExporter, error) {
			e := &fakeExporter{cfg: c}
			cf.built = append(cf.built, e)
			return e, nil
		},
	}
	if mutate != nil {
		mutate(f, cf.svc)
	}
	keys := config.GatedKeys()
	ss.OnChange(keys, cf.svc.validate, cf.svc.apply)
	ss.OnChangeTx(keys, cf.svc.syncTx)
	return cf
}

func (cf *complianceFixture) set(t *testing.T, changes ...ops.SettingChange) (ops.ComplianceSetResult, error) {
	t.Helper()
	return cf.svc.Set(context.Background(), admin, ops.ComplianceSetArgs{Changes: changes})
}

func (cf *complianceFixture) actions(t *testing.T, prefix string) []store.AuditRow {
	t.Helper()
	var out []store.AuditRow
	for _, r := range cf.auditRows() {
		if strings.HasPrefix(r.Action, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func on(t *testing.T, cf *complianceFixture, level, endpoint string) {
	t.Helper()
	if _, err := cf.set(t,
		ops.SettingChange{Key: config.KeyComplianceMonitoring, Value: level},
		ops.SettingChange{Key: config.KeyComplianceEndpoint, Value: endpoint}); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsSetRefusesComplianceKeys(t *testing.T) {
	cf := newComplianceFixture(t, nil)
	for _, key := range config.ComplianceKeys {
		_, err := cf.ss.Set(context.Background(), admin, ops.SettingsSetArgs{
			WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: key, Value: "on"})
		if err == nil || !strings.Contains(err.Error(), "compliance_set") {
			t.Fatalf("%s: err = %v, want a refusal that names compliance_set", key, err)
		}
	}
	if _, ok, _ := cf.st.GetCompliancePolicy(context.Background()); ok {
		t.Fatal("a refused change wrote a policy")
	}
	if cf.svc.Monitor.Policy().Active() {
		t.Fatal("the monitor became active")
	}
}

func TestComplianceSetActivatesWithVersionAndAuditRows(t *testing.T) {
	cf := newComplianceFixture(t, nil)
	on(t, cf, "actions", "http://127.0.0.1:4318")
	row, ok, err := cf.st.GetCompliancePolicy(context.Background())
	if err != nil || !ok || row.Version != 1 || row.Level != "actions" || row.Input || row.DestLabel != "http://127.0.0.1:4318" {
		t.Fatalf("policy = %+v %v %v", row, ok, err)
	}
	if p := cf.svc.Monitor.Policy(); p.Version != 1 || p.Level != "actions" {
		t.Fatalf("monitor = %+v", p)
	}
	if len(cf.built) != 1 {
		t.Fatalf("exporters built = %d", len(cf.built))
	}
	enable := cf.actions(t, AuditComplianceEnable)
	if len(enable) != 1 || enable[0].Target != "v=1 level=actions input=off traffic=metadata dest=http://127.0.0.1:4318" || enable[0].Actor != admin.ID {
		t.Fatalf("enable rows = %+v", enable)
	}
	if eff := cf.actions(t, AuditComplianceEffective); len(eff) != 1 {
		t.Fatalf("effective rows = %+v", eff)
	}
	// The first record of the stream states the policy.
	if r := cf.built[0].records; len(r) != 1 || r[0].Event != compliance.EventPolicy {
		t.Fatalf("first record = %+v", r)
	}

	// Level and input change: one version, one change row.
	if _, err := cf.set(t, ops.SettingChange{Key: config.KeyComplianceMonitoring, Value: "output"},
		ops.SettingChange{Key: config.KeyComplianceInput, Value: "on"}); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := cf.st.GetCompliancePolicy(context.Background()); row.Version != 2 || !row.Input || row.Level != "output" {
		t.Fatalf("policy = %+v", row)
	}
	if n := len(cf.actions(t, AuditComplianceChange)); n != 1 {
		t.Fatalf("change rows = %d", n)
	}
	// Same configuration again: no version, no rows, no second exporter.
	if _, err := cf.set(t, ops.SettingChange{Key: config.KeyComplianceInput, Value: "on"}); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := cf.st.GetCompliancePolicy(context.Background()); row.Version != 2 {
		t.Fatalf("version = %d after a no-op", row.Version)
	}
	// Retarget: the old exporter's queue is discarded, a new one runs.
	if _, err := cf.set(t, ops.SettingChange{Key: config.KeyComplianceEndpoint, Value: "http://127.0.0.1:4319"}); err != nil {
		t.Fatal(err)
	}
	if n := len(cf.actions(t, AuditComplianceRetarget)); n != 1 {
		t.Fatalf("retarget rows = %d", n)
	}
	// Every effective configuration change builds a new exporter and
	// discards the old queue (enable, level/input, retarget: three).
	if !cf.built[0].discarded || !cf.built[1].discarded || len(cf.built) != 3 {
		t.Fatalf("old exporters discarded=%v/%v, built=%d", cf.built[0].discarded, cf.built[1].discarded, len(cf.built))
	}
	// Disable.
	if _, err := cf.set(t, ops.SettingChange{Key: config.KeyComplianceMonitoring, Unset: true},
		ops.SettingChange{Key: config.KeyComplianceInput, Unset: true}); err != nil {
		t.Fatal(err)
	}
	if n := len(cf.actions(t, AuditComplianceDisable)); n != 1 {
		t.Fatalf("disable rows = %d", n)
	}
	if cf.svc.Monitor.Policy().Active() || !cf.built[2].discarded {
		t.Fatal("monitoring is still active or the exporter was kept")
	}
	for _, r := range cf.actions(t, "compliance.") {
		if len(r.Target) > 256 || strings.Contains(r.Target, "secret") {
			t.Fatalf("row %+v", r)
		}
	}
}

func TestComplianceDestinationRules(t *testing.T) {
	cases := []struct {
		name    string
		changes []ops.SettingChange
		want    string
	}{
		{"http to a remote host", []ops.SettingChange{{Key: config.KeyComplianceMonitoring, Value: "actions"}, {Key: config.KeyComplianceEndpoint, Value: "http://collector.example.com:4318"}}, "https"},
		{"remote https needs credentials", []ops.SettingChange{{Key: config.KeyComplianceMonitoring, Value: "actions"}, {Key: config.KeyComplianceEndpoint, Value: "https://collector.example.com:4318"}}, "compliance_headers"},
		{"no endpoint", []ops.SettingChange{{Key: config.KeyComplianceMonitoring, Value: "actions"}}, "compliance_endpoint"},
		{"input without monitoring", []ops.SettingChange{{Key: config.KeyComplianceInput, Value: "on"}}, "compliance_input"},
		{"endpoint with userinfo", []ops.SettingChange{{Key: config.KeyComplianceMonitoring, Value: "actions"}, {Key: config.KeyComplianceEndpoint, Value: "https://u:p@collector.example.com"}}, "compliance_endpoint"},
		{"bad header line", []ops.SettingChange{{Key: config.KeyComplianceMonitoring, Value: "actions"}, {Key: config.KeyComplianceEndpoint, Value: "http://127.0.0.1:4318"}, {Key: config.KeyComplianceHeaders, Value: "no colon here"}}, "line 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cf := newComplianceFixture(t, nil)
			_, err := cf.set(t, c.changes...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
			if strings.Contains(err.Error(), "p@") || strings.Contains(err.Error(), "no colon") {
				t.Fatalf("the error echoes a value: %v", err)
			}
			if _, ok, _ := cf.st.GetCompliancePolicy(context.Background()); ok || len(cf.built) != 0 {
				t.Fatal("a refused change left state behind")
			}
		})
	}
	// Remote https with a header is accepted; loopback http needs none.
	cf := newComplianceFixture(t, nil)
	if _, err := cf.set(t, ops.SettingChange{Key: config.KeyComplianceMonitoring, Value: "actions"},
		ops.SettingChange{Key: config.KeyComplianceEndpoint, Value: "https://collector.example.com:4318/"},
		ops.SettingChange{Key: config.KeyComplianceHeaders, Value: "Authorization: Bearer abc"}); err != nil {
		t.Fatal(err)
	}
	if got := cf.built[0].cfg.LogsURL; got != "https://collector.example.com:4318/v1/logs" {
		t.Fatalf("logs url = %q", got)
	}
}

func TestComplianceRollbackLeavesNoPolicyAndNoSettings(t *testing.T) {
	cf := newComplianceFixture(t, func(f *fixture, s *ComplianceService) {
		s.aud = failingAuditor{settingsAuditAdapter: settingsAuditAdapter{log: audit.New(f.st)}, action: AuditComplianceEnable}
	})
	_, err := cf.set(t, ops.SettingChange{Key: config.KeyComplianceMonitoring, Value: "actions"},
		ops.SettingChange{Key: config.KeyComplianceEndpoint, Value: "http://127.0.0.1:4318"})
	if err == nil {
		t.Fatal("the change went through although its audit row failed")
	}
	if _, ok, _ := cf.st.GetCompliancePolicy(context.Background()); ok {
		t.Fatal("a policy row survived the rollback")
	}
	if _, ok, _ := cf.st.GetSetting(context.Background(), config.KeyComplianceMonitoring); ok {
		t.Fatal("a settings row survived the rollback")
	}
	if cf.svc.Monitor.Policy().Active() || len(cf.built) != 0 {
		t.Fatal("the failed change took effect")
	}
}

func TestComplianceStartupFromEnvironmentLeavesProvenance(t *testing.T) {
	cf := newComplianceFixture(t, nil)
	cf.env["MINION_COMPLIANCE_MONITORING"] = "output"
	cf.env["MINION_COMPLIANCE_ENDPOINT"] = "http://127.0.0.1:4318"
	ss, err := NewSettingsService(context.Background(), SettingsDeps{
		Store: cf.st, Auditor: settingsAuditAdapter{log: audit.New(cf.st)}, Registry: config.Default,
		Live: IsLiveSetting, LookupEnv: cf.env.lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	cf.svc.settings = ss
	if err := cf.svc.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := cf.svc.Monitor.Policy(); p.Version != 1 || p.Level != "output" {
		t.Fatalf("policy = %+v", p)
	}
	rows := cf.actions(t, AuditComplianceStartup)
	if len(rows) != 1 || rows[0].Actor != "environment" || !strings.HasPrefix(rows[0].Target, "v=1 level=output") {
		t.Fatalf("startup rows = %+v", rows)
	}
	// A second start with the same environment bumps nothing but records the start again.
	if err := cf.svc.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := cf.svc.Monitor.Policy(); p.Version != 1 {
		t.Fatalf("version = %d", p.Version)
	}
	if n := len(cf.actions(t, AuditComplianceStartup)); n != 2 {
		t.Fatalf("startup rows = %d", n)
	}
	// With monitoring off and nothing from the environment, a start writes nothing.
	cf2 := newComplianceFixture(t, nil)
	if err := cf2.svc.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(cf2.actions(t, "compliance.")); n != 0 {
		t.Fatalf("rows on a quiet start = %d", n)
	}
}

func TestComplianceExporterFailureIsAuditedAndFailsClosed(t *testing.T) {
	cf := newComplianceFixture(t, func(_ *fixture, s *ComplianceService) {
		s.build = func(config.ComplianceConfig, obs.Process) (ComplianceExporter, error) {
			return nil, errors.New("cannot start")
		}
	})
	_, err := cf.set(t, ops.SettingChange{Key: config.KeyComplianceMonitoring, Value: "actions"},
		ops.SettingChange{Key: config.KeyComplianceEndpoint, Value: "http://127.0.0.1:4318"})
	_ = err // the change is stored and effective as a policy; the export failed
	if n := len(cf.actions(t, AuditComplianceFailed)); n != 1 {
		t.Fatalf("failed rows = %d", n)
	}
	if st := cf.svc.Status(); st == nil || st.LastError == "" {
		t.Fatalf("status = %+v", st)
	}
	// Records that are due while no exporter exists are counted, not lost silently.
	cf.svc.Monitor.Emit(compliance.Record{Event: compliance.EventAction})
	if st := cf.svc.Status(); st.Dropped["no_exporter"] != 1 {
		t.Fatalf("dropped = %+v", st.Dropped)
	}
}

// The payload level of the traffic log is gated like compliance: settings_set
// refuses it, compliance_set accepts it and bumps the policy; metadata and
// off stay open to settings_set, and a form that resends an unchanged payload
// is not a change.
func TestTrafficPayloadIsGated(t *testing.T) {
	cf := newComplianceFixture(t, nil)
	_, err := cf.ss.Set(context.Background(), admin, ops.SettingsSetArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: config.KeyTrafficLog, Value: config.TrafficPayload})
	if err == nil || !strings.Contains(err.Error(), "compliance_set") {
		t.Fatalf("settings_set payload: %v", err)
	}
	if _, err := cf.ss.Set(context.Background(), admin, ops.SettingsSetArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: config.KeyTrafficLog, Value: config.TrafficMetadata}); err != nil {
		t.Fatalf("settings_set metadata: %v", err)
	}
	res, err := cf.set(t, ops.SettingChange{Key: config.KeyTrafficLog, Value: config.TrafficPayload})
	if err != nil {
		t.Fatal(err)
	}
	if res.Policy.Traffic != "payload" || res.Policy.Version != 1 || res.Policy.Level != "off" {
		t.Fatalf("policy = %+v", res.Policy)
	}
	if !cf.svc.Monitor.Policy().TrafficPayload || !cf.svc.Monitor.Policy().Active() {
		t.Fatal("the monitor does not know about the payload level")
	}
	rows := cf.actions(t, AuditComplianceEnable)
	if len(rows) != 1 || !strings.Contains(rows[0].Target, "traffic=payload") {
		t.Fatalf("rows = %+v", rows)
	}
	// Resending the same value through settings_set is not a change.
	if _, err := cf.ss.Set(context.Background(), admin, ops.SettingsSetArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: config.KeyTrafficLog, Value: config.TrafficPayload}); err != nil {
		t.Fatalf("an unchanged payload value was refused: %v", err)
	}
	// Stepping down is open, and it is an effective change of the policy.
	if _, err := cf.ss.Set(context.Background(), admin, ops.SettingsSetArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: config.KeyTrafficLog, Value: config.TrafficMetadata}); err != nil {
		t.Fatal(err)
	}
	if p := cf.svc.Monitor.Policy(); p.TrafficPayload || p.Version != 2 {
		t.Fatalf("policy = %+v", p)
	}
	if n := len(cf.actions(t, AuditComplianceDisable)); n != 1 {
		t.Fatalf("disable rows = %d", n)
	}
	// compliance_set changes nothing but the gated keys.
	if _, err := cf.set(t, ops.SettingChange{Key: config.KeyLogLevel, Value: "debug"}); err == nil {
		t.Fatal("compliance_set accepted an ordinary key")
	}
}
