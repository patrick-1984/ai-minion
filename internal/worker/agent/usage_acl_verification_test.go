package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func TestUsageTelemetryPreservesUnsharedConsoleRestrictions(t *testing.T) {
	e := newEnv(t)
	e.policy.SetWorkerID(selfWorker)
	e.ex.registerUsage(e.st)
	ctx := context.Background()
	hub, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	registration := store.Enrollment{HubPub: hub.Public(), WorkerID: selfWorker, Owner: "synthetic-owner"}
	if err := e.st.SaveEnrollment(ctx, registration); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shared", "unshared"} {
		ref := e.host.addSession(name, "codex")
		e.host.setScreen(ref, "synthetic console contents")
	}
	e.host.sessions[1].Task = "synthetic private task text"
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"shared"}}); err != nil {
		t.Fatal(err)
	}
	args := ops.WorkerUsageArgs{WorkerID: selfWorker}
	_, err = e.call(ops.OpWorkerUsage, allActions, args)
	wantAPIError(t, err, "forbidden", "")
	if err := e.st.AcceptUsageRegistration(ctx, registration.HubPub, registration.WorkerID, registration.Owner); err != nil {
		t.Fatal(err)
	}
	raw, err := e.call(ops.OpWorkerUsage, allActions, args)
	if err != nil {
		t.Fatal(err)
	}
	var report ops.UsageReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 2 || !slices.ContainsFunc(report.Sessions, func(s ops.UsageSession) bool { return s.Name == "unshared" }) {
		t.Fatalf("usage report did not cover unshared sessions: %+v", report.Sessions)
	}
	if strings.Contains(string(raw), "synthetic console contents") || strings.Contains(string(raw), "synthetic private task text") {
		t.Fatal("usage reporting exposed console content or task text")
	}

	if listed := e.list(); len(listed) != 1 || listed[0].Name != "shared" {
		t.Fatalf("ordinary session list after telemetry: %+v", listed)
	}
	_, err = e.call(ops.OpSessionSnapshot, allActions, ops.SessionSnapshotArgs{Target: target("unshared")})
	wantAPIError(t, err, "forbidden", "not_shared")
	_, err = e.call(ops.OpSessionCapture, allActions, ops.SessionCaptureArgs{Target: target("unshared")})
	wantAPIError(t, err, "forbidden", "not_shared")
	_, err = e.write(ops.OpSendText, sendText("unshared", "synthetic input", true))
	wantAPIError(t, err, "forbidden", "preview_only")
	if len(e.host.pastes()) != 0 {
		t.Fatal("input reached an unshared console")
	}
	args.WorkerID = otherWorker
	_, err = e.call(ops.OpWorkerUsage, allActions, args)
	wantAPIError(t, err, "forbidden", "")
}
