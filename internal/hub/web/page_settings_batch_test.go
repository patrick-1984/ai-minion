package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// A batch section (Log export) posts one settings_set with all its changes:
// an empty secret field keeps the stored secret, "remove" clears it, an empty
// plain field returns to the default.
func TestSettingsSectionSavesOneBatch(t *testing.T) {
	s, f := settingsServer(t)
	f.reply(ops.OpSettingsSet, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	form := url.Values{
		"section":           {"Log export"},
		"v.otel":            {"on"},
		"v.otel_endpoint":   {"https://collector.example:4318"},
		"v.syslog_endpoint": {""},
		"v.otel_headers":    {""},
		"s.otel_headers":    {"1"},
		"v.other_secret":    {""},
		"s.other_secret":    {"1"},
		"r.other_secret":    {"1"},
	}
	rec := post(t, s, pathAdminSettingsSection, adminHeader, form)
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	calls := f.called(ops.OpSettingsSet)
	if len(calls) != 1 {
		t.Fatalf("%d settings_set calls, want one batch", len(calls))
	}
	changes, _ := calls[0].Args["changes"].([]any)
	got := map[string]map[string]any{}
	for _, c := range changes {
		m := c.(map[string]any)
		got[m["key"].(string)] = m
	}
	if len(got) != 4 || got["otel"]["value"] != "on" || got["otel_endpoint"]["value"] != "https://collector.example:4318" ||
		got["syslog_endpoint"]["unset"] != true || got["other_secret"]["unset"] != true {
		t.Fatalf("changes = %v", got)
	}
	if _, ok := got["otel_headers"]; ok {
		t.Fatal("an empty secret field must keep the stored secret")
	}
	if calls[0].Args["key"] != nil {
		t.Fatalf("a batch carries no single key: %v", calls[0].Args)
	}
}

func logExportSettings() ops.SettingsListResult {
	return ops.SettingsListResult{Settings: []ops.SettingInfo{
		{Key: "otel", Env: "MINION_OTEL", Value: "on", Source: ops.SettingSourcePanel, Live: true, Choices: []string{"off", "on"},
			Section: "Log export", Revision: "rev-otel"},
		{Key: "otel_endpoint", Env: "MINION_OTEL_ENDPOINT", Value: "https://c.example", Source: ops.SettingSourcePanel, Live: true,
			Section: "Log export", Revision: "rev-endpoint"},
		{Key: "otel_headers", Env: "MINION_OTEL_HEADERS", EnvFile: "MINION_OTEL_HEADERS_FILE", Secret: true, Value: "[redacted]",
			Source: ops.SettingSourcePanel, Live: true, Section: "Log export", Revision: "rev-headers"},
	}}
}

// The Log export section carries each row's revision (so that a stale page
// cannot restore an old endpoint next to a new credential), a multi-line
// field for the header list, and the effective state of the sinks.
func TestLogExportSectionRevisionsTextareaAndStatus(t *testing.T) {
	f := newFakeOps().reply(ops.OpSettingsList, logExportSettings()).reply(ops.OpHubDiagnostics, ops.HubDiagnosticsResult{
		Sinks: []ops.LogSinkStatus{{Name: "otel", Generation: 3, LastError: "export failed: 401 Unauthorized"}},
	})
	connect(t, f)
	s := adminServer(t)
	body := get(t, s, pathAdminSettings, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `name="rev.otel_endpoint" value="rev-endpoint"`, `<textarea id="v-otel_headers" name="v.otel_headers"`,
		"Log export (otel)", "off (configuration 3); last error: export failed: 401 Unauthorized")
	mustNotHave(t, body, "[redacted]")

	f.reply(ops.OpSettingsSet, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	rec := post(t, s, pathAdminSettingsSection, adminHeader, url.Values{
		"v.otel_endpoint": {"https://c.example"}, "rev.otel_endpoint": {"rev-endpoint"},
		"v.otel_headers": {"A: 1\r\nB: 2"}, "s.otel_headers": {"1"}, "rev.otel_headers": {"rev-headers"},
	})
	if rec.Code != 200 {
		t.Fatalf("POST = %d", rec.Code)
	}
	changes := f.called(ops.OpSettingsSet)[0].Args["changes"].([]any)
	got := map[string]map[string]any{}
	for _, c := range changes {
		m := c.(map[string]any)
		got[m["key"].(string)] = m
	}
	if got["otel_endpoint"]["revision"] != "rev-endpoint" || got["otel_headers"]["value"] != "A: 1\nB: 2" || got["otel_headers"]["revision"] != "rev-headers" {
		t.Fatalf("changes = %v", got)
	}

	// A kept (blank) secret is sent as an expectation with its revision.
	rec = post(t, s, pathAdminSettingsSection, adminHeader, url.Values{
		"v.otel_endpoint": {"https://other.example"}, "rev.otel_endpoint": {"rev-endpoint"},
		"v.otel_headers": {""}, "s.otel_headers": {"1"}, "rev.otel_headers": {"rev-headers"},
	})
	if rec.Code != 200 {
		t.Fatalf("POST = %d", rec.Code)
	}
	calls := f.called(ops.OpSettingsSet)
	exp, _ := calls[len(calls)-1].Args["expect"].([]any)
	if len(exp) != 1 || exp[0].(map[string]any)["key"] != "otel_headers" || exp[0].(map[string]any)["revision"] != "rev-headers" {
		t.Fatalf("expect = %v", calls[len(calls)-1].Args)
	}
}

func TestDiagnosticsShowsTheSinks(t *testing.T) {
	d := sampleDiagnostics()
	d.Sinks = []ops.LogSinkStatus{{Name: "syslog", Enabled: true, Destination: "tls://logs.example:6514", Level: "warn", Generation: 1, Sent: 9}}
	f := newFakeOps().reply(ops.OpHubDiagnostics, d)
	connect(t, f)
	body := get(t, adminServer(t), pathAdminDiagnostics, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "<dt>Log export (syslog)</dt><dd>tls://logs.example:6514 at warn and above; sent 9, queued 0, dropped 0 (configuration 1)</dd>")
}

func complianceSettings() ops.SettingsListResult {
	return ops.SettingsListResult{Settings: []ops.SettingInfo{
		{Key: "compliance_monitoring", Env: "MINION_COMPLIANCE_MONITORING", Value: "off", Source: ops.SettingSourceDefault, Live: true,
			Choices: []string{"off", "actions", "output"}, Section: "Compliance monitoring", Revision: "rev-m"},
		{Key: "compliance_endpoint", Env: "MINION_COMPLIANCE_ENDPOINT", Value: "", Source: ops.SettingSourceDefault, Live: true,
			Section: "Compliance monitoring", Revision: "rev-e"},
		{Key: "worker_traffic_log", Env: "MINION_WORKER_TRAFFIC_LOG", Value: "metadata", Source: ops.SettingSourcePanel, Live: true,
			Choices: []string{"off", "metadata"}, Section: "Log export", Revision: "rev-t"},
	}}
}

// The compliance section is its own card: it posts to compliance_set, never to
// settings_set, says plainly what is recorded, and also holds the traffic log
// level (whose payload value is gated).
func TestComplianceSectionPostsComplianceSetAndSaysWhatIsRecorded(t *testing.T) {
	f := newFakeOps().reply(ops.OpSettingsList, complianceSettings()).reply(ops.OpHubDiagnostics, ops.HubDiagnosticsResult{})
	connect(t, f)
	s := adminServer(t)
	body := get(t, s, pathAdminSettings, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `hx-post="/admin/compliance-section"`, "text sent through Minion is recorded as sent", "echo-off keeps typed passwords out of screens",
		`name="v.compliance_monitoring"`)
	if n := strings.Count(body, `name="v.worker_traffic_log"`); n != 2 {
		t.Fatalf("the traffic log row must be in the Log export and the Compliance section, found %d", n)
	}

	f.reply(ops.OpComplianceSet, ops.ComplianceSetResult{Policy: ops.CompliancePolicy{Version: 2, Level: "actions"}})
	rec := post(t, s, pathAdminComplianceSection, adminHeader, url.Values{
		"v.compliance_monitoring": {"actions"}, "rev.compliance_monitoring": {"rev-m"},
		"v.compliance_endpoint": {"http://127.0.0.1:4318"},
	})
	if rec.Code != 200 || rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("POST = %d %v", rec.Code, rec.Header())
	}
	if n := len(f.called(ops.OpSettingsSet)); n != 0 {
		t.Fatalf("settings_set was called %d times", n)
	}
	calls := f.called(ops.OpComplianceSet)
	if len(calls) != 1 || len(calls[0].Args["changes"].([]any)) != 2 {
		t.Fatalf("compliance_set calls = %+v", calls)
	}
}

// A sign-in older than five minutes: the answer says where to confirm it.
func TestComplianceSectionAsksForAFreshSignIn(t *testing.T) {
	f := newFakeOps().reply(ops.OpSettingsList, complianceSettings()).reply(ops.OpHubDiagnostics, ops.HubDiagnosticsResult{})
	f.on(ops.OpComplianceSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: "reauth_required", Message: "Sign in again"}
	})
	connect(t, f)
	s := adminServer(t)
	rec := post(t, s, pathAdminComplianceSection, adminHeader, url.Values{"v.compliance_monitoring": {"actions"}})
	mustHave(t, rec.Body.String(), "/admin/identity", "last five minutes")
	if rec.Header().Get("HX-Refresh") != "" {
		t.Fatal("a refused save must keep the form")
	}
}

// The compliance header list is a multi-line secret field: two header lines
// survive the form (an input would strip the line break).
func TestComplianceHeadersAreAMultilineField(t *testing.T) {
	list := complianceSettings()
	list.Settings = append(list.Settings, ops.SettingInfo{Key: "compliance_headers", Env: "MINION_COMPLIANCE_HEADERS", EnvFile: "MINION_COMPLIANCE_HEADERS_FILE",
		Secret: true, Value: "[redacted]", Source: ops.SettingSourcePanel, Live: true, Section: "Compliance monitoring", Revision: "rev-h"})
	f := newFakeOps().reply(ops.OpSettingsList, list).reply(ops.OpHubDiagnostics, ops.HubDiagnosticsResult{})
	connect(t, f)
	s := adminServer(t)
	mustHave(t, get(t, s, pathAdminSettings, "X-Test-Session", adminHeader).Body.String(), `<textarea id="v-compliance_headers" name="v.compliance_headers"`)
	f.reply(ops.OpComplianceSet, ops.ComplianceSetResult{})
	post(t, s, pathAdminComplianceSection, adminHeader, url.Values{
		"v.compliance_headers": {"Authorization: Bearer x\r\nX-Tenant: a"}, "s.compliance_headers": {"1"}})
	calls := f.called(ops.OpComplianceSet)
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	ch := calls[0].Args["changes"].([]any)[0].(map[string]any)
	if ch["value"] != "Authorization: Bearer x\nX-Tenant: a" {
		t.Fatalf("value = %q", ch["value"])
	}
}
