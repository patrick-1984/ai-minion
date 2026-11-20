package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

type upStatusReader func(context.Context, string, *ownerSetup) (agent.StatusReport, error)

// readUpStatus uses the same keys, enrollment and state-directory-checked live
// socket as worker status. Under sudo both disk and socket are read as the
// target account, with its environment and no invitation in the child.
func readUpStatus(ctx context.Context, stateDir string, owner *ownerSetup) (agent.StatusReport, error) {
	if owner == nil {
		deps := defaultWorkerStatusDeps()
		deps.sessions = nil
		return deps.report(ctx, config.MapSource{config.KeyStateDir: stateDir})
	}
	var r agent.StatusReport
	var buf bytes.Buffer
	args := []string{"worker", "status", "--state-dir", stateDir, "--json"}
	if err := runAsUser(ctx, owner.Target, owner.Binary, args, &buf); err != nil {
		return r, fmt.Errorf("could not read status as %s: %w", owner.Target.User, err)
	}
	err := json.Unmarshal(buf.Bytes(), &r)
	return r, err
}

func showUpStatus(ctx context.Context, out io.Writer, stateDir string, owner *ownerSetup, read upStatusReader, commands []string) error {
	report, err := waitForWorkerStatus(ctx, func(ctx context.Context) (agent.StatusReport, error) {
		return read(ctx, stateDir, owner)
	})
	report.WriteStartupSummary(out, commands)
	return err
}

func waitForWorkerStatus(ctx context.Context, read func(context.Context) (agent.StatusReport, error)) (agent.StatusReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var last agent.StatusReport
	for ctx.Err() == nil {
		r, err := read(ctx)
		if err != nil {
			return last, err
		}
		last = r
		if r.Connection.Live && (r.Mode == agent.ModeSolo || r.Connection.State == string(agent.ConnConnected) ||
			r.Connection.State == string(agent.ConnBackoff) || r.PairingState == agent.PairingPending || r.Paused ||
			(r.Connection.State == string(agent.ConnIdle) && r.Connection.Reason != "")) {
			return r, nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(300 * time.Millisecond):
		}
	}
	if !last.Connection.Live {
		return last, fmt.Errorf("the Worker did not answer on its local socket: %w", ctx.Err())
	}
	return last, nil
}

func upNextCommands(det service.Detection, target service.Target, owned, pinState bool, stateDir string) []string {
	minion := "minion worker "
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	if det.Manager == service.ManagerWindows {
		quote = func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	}
	if owned {
		minion = "sudo -H -u " + quote(target.User) + " -- " + minion
	} else if target.UID == 0 && os.Getenv("SUDO_USER") != "" && det.Manager != service.ManagerWindows {
		// Root's Worker, set up through sudo: the commands run as root too.
		minion = "sudo " + minion
	}
	status := minion + "status"
	if pinState {
		status += " --state-dir " + quote(stateDir)
	}
	if det.Manager == service.ManagerSystemd && det.Scope == service.ScopeSystem {
		unit := quote(service.SystemUnitFor(target.User))
		return []string{status, "sudo systemctl stop " + unit, "sudo systemctl start " + unit}
	}
	return []string{status, minion + "down", minion + "start"}
}
