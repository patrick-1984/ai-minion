package web

import (
	"net/url"
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
