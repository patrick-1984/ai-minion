package cli

import (
	"context"
	"encoding/json"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"io"
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

// Gate-1 C: "config share" never opens worker.db while a Worker holds the
// lifecycle lock; it goes through the running Worker.
func TestConfigShareGoesThroughTheRunningWorker(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	cd.lockState = func(string) (io.Closer, error) { return nil, agent.ErrStateInUse }
	cd.openStore = func(context.Context, string) (shareStore, error) {
		t.Fatal("worker.db opened while a Worker runs")
		return nil, nil
	}
	sd := defaultWorkerShareDeps()
	var applied agent.LocalShareApplyArgs
	sd.call = func(_ context.Context, op string, args any) (json.RawMessage, error) {
		switch {
		case op == agent.OpLocalStatus:
			return json.Marshal(agent.LiveStatusReply{StateDir: dir, Instance: "i"})
		case strings.Contains(op, agent.OpLocalShareList):
			return json.Marshal(agent.ShareListResult{Rule: "all", Revision: 3})
		}
		applied = args.(agent.LocalShareApplyArgs)
		return json.Marshal(agent.ShareListResult{Rule: "none", Revision: 4})
	}
	cd.share = &sd
	_, _, err := runWorkerCmds(t, testStatusDeps(nil), cd, "worker", "config", "share", "none", "--state-dir", dir)
	if err != nil || applied.Mode != "none" || applied.IfRevision != 3 {
		t.Fatalf("err %v applied %+v", err, applied)
	}
}
