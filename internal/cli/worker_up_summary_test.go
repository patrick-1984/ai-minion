package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

func TestWorkerUpReportsLiveStateAndNextCommands(t *testing.T) {
	f := newUpFixture(t, false)
	f.deps.readStatus = func(_ context.Context, dir string, owner *ownerSetup) (agent.StatusReport, error) {
		if dir != f.stateDir() || owner != nil {
			t.Fatalf("summary lookup got %q, owner %+v", dir, owner)
		}
		return agent.StatusReport{Mode: agent.ModeSolo, Connection: agent.StatusConnection{Live: true}}, nil
	}
	out, err := f.run("--share-existing", "none")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"State: running (Solo)", "minion worker status", "minion worker down", "minion worker start"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWorkerUpReportsPendingApprovalOnEachManager(t *testing.T) {
	for _, manager := range []service.Manager{service.ManagerSystemd, service.ManagerLaunchd, service.ManagerWindows} {
		t.Run(string(manager), func(t *testing.T) {
			f := newUpFixture(t, manager == service.ManagerSystemd)
			switch manager {
			case service.ManagerLaunchd:
				f.deps.detectEnv = service.Env{GOOS: "darwin"}
			case service.ManagerWindows:
				f, _ = newWinUp(t)
			}
			hub, worker := "whale-blue-winter-tree-north-tulip", "valley-post-lemon-fern-moon-lake"
			f.deps.readStatus = func(_ context.Context, dir string, owner *ownerSetup) (agent.StatusReport, error) {
				if manager == service.ManagerSystemd {
					if owner == nil || owner.Target.User != "ann" || owner.StateDir != dir {
						t.Fatalf("wrong summary account %+v for %s", owner, dir)
					}
				} else if owner != nil {
					t.Fatalf("unexpected target account: %+v", owner)
				}
				return agent.StatusReport{
					Mode: agent.ModeHub, Connection: agent.StatusConnection{State: string(agent.ConnConnected), Live: true},
					Approval: agent.ApprovalPending, HubFingerprintWords: hub, WorkerFingerprintWords: worker,
				}, nil
			}
			out, err := f.run("--share-existing", "none")
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"State: waiting for approval", "Hub key: " + hub, "Worker fingerprint: " + worker}
			if manager == service.ManagerSystemd {
				want = append(want, "sudo -H -u 'ann' -- minion worker status", "sudo systemctl stop 'minion-worker@ann.service'", "sudo systemctl start 'minion-worker@ann.service'")
			}
			for _, line := range want {
				if !strings.Contains(out, line) {
					t.Errorf("missing %q in %s", line, out)
				}
			}
		})
	}
}

func TestWorkerStatusWaitUsesStoredKeysAndLiveWelcome(t *testing.T) {
	dir := t.TempDir()
	hub, worker := enrollWorker(t, dir)
	deps := testStatusDeps(map[string]string{"MINION_SERVER": "https://hub.example"})
	calls := 0
	deps.live = func(string) agent.LiveSource {
		calls++
		if calls == 1 {
			return fakeLive{err: agent.ErrNoLiveWorker}
		}
		st := agent.LiveStatus{ApprovalKnown: true, Connector: agent.Status{State: agent.ConnConnected}}
		st.Connector.Welcome.Pending = calls == 2
		return fakeLive{ls: st}
	}
	run := func() string {
		out, _, err := runWorkerCmds(t, deps, workerConfigDeps{}, "worker", "status", "--wait", "--state-dir", dir)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := run()
	for _, want := range []string{"State: waiting for approval", "Hub key: " + identity.Fingerprint(hub).Words(),
		"Worker fingerprint: " + identity.Fingerprint(worker).Words()} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	out = run()
	if !strings.Contains(out, "State: connected") || !strings.Contains(out, "Hub key: "+identity.Fingerprint(hub).Words()) ||
		strings.Contains(out, "waiting for approval") {
		t.Fatalf("approved connection: %s", out)
	}
}

func TestWorkerUpBoundsAbsentLiveStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	err := showUpStatus(ctx, &out, "elsewhere", nil, func(context.Context, string, *ownerSetup) (agent.StatusReport, error) {
		return agent.StatusReport{Mode: agent.ModeHub, Connection: agent.StatusConnection{State: agent.ConnUnknown}}, nil
	}, []string{"minion worker status"})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(out.String(), "live status unavailable") ||
		strings.Contains(out.String(), "waiting for approval") {
		t.Fatalf("err %v, output %q", err, out.String())
	}
}

func TestWorkerUpMigrationLeavesOtherWorkersRunning(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, mode, state string
		wantErr           bool
	}{
		{"system service", agent.RunModeSystemd, dir, false},
		{"launch agent", agent.RunModeLaunchd, dir, false},
		{"different foreground profile", agent.RunModeForeground, t.TempDir(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(agent.LiveStatusReply{StateDir: tc.state, RunMode: tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			b := &fakeBackend{raw: raw}
			err = stopPriorWorker(context.Background(), dir, true, b)
			if (err != nil) != tc.wantErr || b.op != agent.OpLocalStatus {
				t.Fatalf("last op %s error %v", b.op, err)
			}
		})
	}
}
