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
