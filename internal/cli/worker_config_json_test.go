package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// Gate-1 C: config set allow_new_sessions --json is one result object with
// the effective value and sources, and a distinct code for 22.
func TestConfigSetBlockJSON(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	out, _, err := runWorkerCmds(t, testStatusDeps(nil), cd, "worker", "config", "set", "allow_new_sessions", "off", "--state-dir", dir, "--json")
	var m map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); jerr != nil {
		t.Fatalf("out %q err %v", out, err)
	}
	if m["ok"] != true || m["effective"] != "off" || m["saved"] != "off" {
		t.Fatalf("block: %v", m)
	}
	// The environment pins it: the unblock is saved but not in force.
	cd = testConfigDeps(map[string]string{"MINION_ALLOW_NEW_SESSIONS": "off"})
	out, _, err = runWorkerCmds(t, testStatusDeps(nil), cd, "worker", "config", "set", "allow_new_sessions", "on", "--state-dir", dir, "--json")
	m = nil
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); jerr != nil || err == nil {
		t.Fatalf("out %q err %v", out, err)
	}
	if m["ok"] != false || m["code"] != "saved_overridden" || m["effective"] != "off" || m["sources"] == nil {
		t.Fatalf("unblock: %v", m)
	}
}
