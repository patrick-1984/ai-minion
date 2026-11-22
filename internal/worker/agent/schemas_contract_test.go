package agent

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// The published schemas of the read-back feature (docs/schemas) are contracts
// for the publisher, the Hub projection and the readers. These tests pin the
// fixtures and the rules that keep a public record public-safe.

func decodeAny(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func fixturePath(name string) []string {
	return []string{"..", "..", "..", "docs", "schemas", "fixtures", name}
}

func TestSchemaFixturesValidate(t *testing.T) {
	for schema, fixtures := range map[string][]string{
		"presence.schema.json":     {"presence_user_running.json", "presence_root_stopped.json", "presence_solo.json"},
		"usage-meters.schema.json": {"usage_meters_full.json", "usage_meters_empty.json"},
	} {
		r := loadSchema(t, schema)
		for _, f := range fixtures {
			if err := r.Validate(decodeAny(t, readFixture(t, fixturePath(f)...))); err != nil {
				t.Errorf("%s does not match %s: %v", f, schema, err)
			}
		}
	}
}

func TestPresenceSchemaRejectsUnsafeRecords(t *testing.T) {
	r := loadSchema(t, "presence.schema.json")
	base := readFixture(t, fixturePath("presence_user_running.json")...)
	mutate := func(f func(m map[string]any)) map[string]any {
		m := decodeAny(t, base)
		f(m)
		return m
	}
	bad := map[string]map[string]any{
		"origin path":        mutate(func(m map[string]any) { m["hub_origin"] = "https://hub.example/proxy/tok" }),
		"origin query":       mutate(func(m map[string]any) { m["hub_origin"] = "https://hub.example?token=s3cret" }),
		"origin fragment":    mutate(func(m map[string]any) { m["hub_origin"] = "https://hub.example#frag" }),
		"origin userinfo":    mutate(func(m map[string]any) { m["hub_origin"] = "https://user:pw@hub.example" }),
		"origin trailing /":  mutate(func(m map[string]any) { m["hub_origin"] = "https://hub.example/" }),
		"origin scheme":      mutate(func(m map[string]any) { m["hub_origin"] = "ftp://hub.example" }),
		"owner name":         mutate(func(m map[string]any) { m["owner"] = "alice" }),
		"worker name":        mutate(func(m map[string]any) { m["worker"] = map[string]any{"name": "laptop"} }),
		"bad scope":          mutate(func(m map[string]any) { m["os_scope"] = "system" }),
		"bad state":          mutate(func(m map[string]any) { m["state"] = "starting" }),
		"wrong schema":       mutate(func(m map[string]any) { m["schema"] = "minion.presence/2" }),
		"missing instance":   mutate(func(m map[string]any) { delete(m, "instance") }),
		"negative heartbeat": mutate(func(m map[string]any) { m["stale_after_s"] = 0 }),
	}
	for name, doc := range bad {
		if err := r.Validate(doc); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestUsageMetersSchemaRejectsBadDocuments(t *testing.T) {
	r := loadSchema(t, "usage-meters.schema.json")
	base := readFixture(t, fixturePath("usage_meters_full.json")...)
	mut := func(f func(slot, meter map[string]any)) map[string]any {
		m := decodeAny(t, base)
		slot := m["slots"].([]any)[0].(map[string]any)
		f(slot, slot["meters"].([]any)[0].(map[string]any))
		return m
	}
	bad := map[string]map[string]any{
		"member ids":   mut(func(s, _ map[string]any) { s["member_ids"] = []any{"u1"} }),
		"session name": mut(func(_, m map[string]any) { m["session"] = "private-build" }),
		"bad unit":     mut(func(_, m map[string]any) { m["unit"] = "bananas" }),
		"percent 101":  mut(func(_, m map[string]any) { m["remaining_percent"] = 101 }),
		"no expiry":    mut(func(_, m map[string]any) { delete(m, "expires_at") }),
		"no coverage":  mut(func(_, m map[string]any) { delete(m, "coverage") }),
		"bad scope":    mut(func(s, _ map[string]any) { s["slot"].(map[string]any)["scope"] = "global" }),
		"empty label":  mut(func(s, _ map[string]any) { s["slot"].(map[string]any)["label"] = "" }),
		"bad pace":     mut(func(_, m map[string]any) { m["pace"] = "fast" }),
		"17 meters": mut(func(s, m map[string]any) {
			ms := make([]any, 17)
			for i := range ms {
				ms[i] = m
			}
			s["meters"] = ms
		}),
		"5 slots": func() map[string]any {
			m := decodeAny(t, base)
			s := m["slots"].([]any)
			m["slots"] = []any{s[0], s[0], s[0], s[0], s[0]}
			return m
		}(),
	}
	for name, doc := range bad {
		if err := r.Validate(doc); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSchemasAreValidJSONWithIDs(t *testing.T) {
	for _, n := range []string{"local-status-daemon", "presence", "usage-meters"} {
		raw := readFixture(t, "..", "..", "..", "docs", "schemas", n+".schema.json")
		m := decodeAny(t, raw)
		id, _ := m["$id"].(string)
		if !strings.HasSuffix(id, "/"+filepath.Base(n)+".schema.json") || m["$schema"] == nil {
			t.Errorf("%s: $id %q", n, id)
		}
	}
}
