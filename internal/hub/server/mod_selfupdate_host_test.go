package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/selfupdate/truenas"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestHostUpdaterPolicyAndQueue(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	h := &hostUpdater{requestFile: filepath.Join(dir, "request.json"), statusFile: filepath.Join(dir, "status.json")}
	if err := h.queue("v0.16.0", "v0.17.0", now); err == nil {
		t.Fatal("queued without host status")
	}
	s := truenas.Status{App: "ai-minion", UpdatedAt: now, Window: "03-05", Channel: "latest", AllowHubPolicy: true, Tick: "disabled"}
	if err := truenas.WriteStatus(h.statusFile, s); err != nil {
		t.Fatal(err)
	}
	args := ops.HubUpdatePolicyArgs{Enabled: true, Window: "22-02", Channel: "0.17"}
	if err := h.policy(args, now); err != nil {
		t.Fatal(err)
	}
	if err := h.policy(args, now); err == nil {
		t.Fatal("stale browser overwrote newer policy")
	}
	if err := h.queue("v0.16.0", "v0.17.0", now); err != nil {
		t.Fatal(err)
	}
	if err := h.queue("v0.16.0", "v0.18.0", now); err == nil {
		t.Fatal("overwrote a pending update")
	}
	r, err := truenas.ReadRequest(h.requestFile, now)
	if err != nil || r.Once.PolicyRevision != 1 || r.Once.Target != "v0.17.0" || r.Policy.Window != "22-02" {
		t.Fatalf("bad request %+v: %v", r, err)
	}
	// An expired click must not disable an independently armed recurring policy.
	view := h.read(now.Add(2 * time.Hour))
	if !view.Enabled || view.PendingRequest != "" {
		t.Fatalf("expiry lost recurring policy: %+v", view)
	}
	args.Enabled, args.Revision = false, 1
	if err := h.policy(args, now); err != nil {
		t.Fatal(err)
	}
	r, err = truenas.ReadRequest(h.requestFile, now)
	if err != nil || r.Once != nil || r.Policy.Enabled {
		t.Fatalf("disable retained queued request: %+v %v", r, err)
	}
	// An online status must opt in before the Hub can arm host policy.
	s.AllowHubPolicy = false
	if err := truenas.WriteStatus(h.statusFile, s); err != nil {
		t.Fatal(err)
	}
	args.Enabled, args.Revision = true, 2
	if err := h.policy(args, now); err == nil {
		t.Fatal("armed a host that refuses Hub policy")
	}
}

func TestHostUpdaterRefusesStaleStatus(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	h := &hostUpdater{requestFile: filepath.Join(dir, "request.json"), statusFile: filepath.Join(dir, "status.json")}
	if err := truenas.WriteStatus(h.statusFile, truenas.Status{App: "ai-minion", UpdatedAt: now.Add(-time.Hour), Tick: "disabled", AllowHubPolicy: true}); err != nil {
		t.Fatal(err)
	}
	if !h.read(now).Stale {
		t.Fatal("stale status reported fresh")
	}
	if err := h.queue("v0.16.0", "v0.17.0", now); err == nil {
		t.Fatal("queued through stale host")
	}
	if err := h.policy(ops.HubUpdatePolicyArgs{Enabled: true, Window: "03-05", Channel: "latest"}, now); err == nil {
		t.Fatal("armed through stale host")
	}
}
