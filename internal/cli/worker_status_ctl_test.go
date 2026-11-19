package cli

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// Gate-1 C: status --json is a result object (ok, code, message) beside the
// status fields.
func TestStatusJSONIsAResultObject(t *testing.T) {
	out, _, err := runWorkerCmds(t, testStatusDeps(nil), workerConfigDeps{}, "worker", "status", "--json", "--state-dir", t.TempDir())
	var m map[string]any
	if err != nil || json.Unmarshal([]byte(out), &m) != nil || m["ok"] != true || m["code"] != "ok" || m["new_sessions"] == nil {
		t.Fatalf("status = %q, %v", out, err)
	}
}

// A config error still prints one object with ok:false.
func TestStatusJSONConfigErrorIsAnObject(t *testing.T) {
	sd := testStatusDeps(nil)
	out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", t.TempDir(), "--server", "not a url")
	var m map[string]any
	if jerr := json.Unmarshal([]byte(out), &m); jerr != nil || m["ok"] != false || err == nil {
		t.Fatalf("status = %q, %v", out, err)
	}
}

// The block shown is the daemon's, not this shell's.
func TestStatusShowsTheDaemonsBlock(t *testing.T) {
	sd := testStatusDeps(nil)
	yes := true
	sd.live = func(string) agent.LiveSource {
		return fakeLive{ls: agent.LiveStatus{NewSessionsBlocked: &yes}}
	}
	out, _, _ := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", t.TempDir())
	var m struct {
		NS struct {
			Blocked bool     `json:"blocked"`
			Sources []string `json:"sources"`
			InForce *bool    `json:"in_force"`
		} `json:"new_sessions"`
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil || !m.NS.Blocked || len(m.NS.Sources) == 0 || m.NS.InForce == nil || !*m.NS.InForce {
		t.Fatalf("status = %q, %v", out, err)
	}
}

// A failing live call makes enforcement unknown, never "in force".
func TestStatusUnknownEnforcementWhenWorkerCannotAnswer(t *testing.T) {
	sd := testStatusDeps(nil)
	sd.live = func(string) agent.LiveSource { return fakeLive{err: errors.New("hung")} }
	out, _, _ := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", t.TempDir())
	var m struct {
		NS struct {
			InForce *bool `json:"in_force"`
		} `json:"new_sessions"`
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil || m.NS.InForce != nil {
		t.Fatalf("status = %q, %v", out, err)
	}
}
