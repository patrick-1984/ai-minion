//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

func TestWorkerUpMigratesForegroundToSystemd(t *testing.T) {
	srv := tmuxtest.New(t)
	f := newUpFixture(t, false)
	f.deps.stopForeground = stopForegroundForService
	state := f.stateDir()
	f.env["MINION_STATE_DIR"] = state
	f.env["MINION_TMUX_SOCKET"] = srv.Socket()
	f.env["MINION_SHARE_EXISTING"] = "none"
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("XPC_SERVICE_NAME", "")
	t.Setenv("MINION_DETACHED", "")
	runProductionWorker(t, f.env)
	if r := productionStatus(t, f.env, state, true); !r.Connection.Live {
		t.Fatal("foreground Worker never started")
	}
	f.runner.answer = func(line string) (string, error) {
		switch {
		case strings.Contains(line, " restart ") && !workerStateFree(state):
			return "", errors.New("the foreground Worker still holds the profile")
		case strings.Contains(line, "is-active"):
			return "active\n", nil
		case strings.HasPrefix(line, "loginctl show-user"):
			return "Linger=yes\n", nil
		}
		return "", nil
	}
	if _, err := f.run(); err != nil {
		t.Fatalf("up: %v", err)
	}
}

// Exercise the privilege handoff's real argument list and environment without
// requiring root or a second account on the test host.
func TestWorkerUpReadsStatusInOwnerEnvironment(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "a profile")
	bin := filepath.Join(home, "minion")
	script := `#!/bin/sh
test "$USER" = owner && test "$PWD" = "$HOME" &&
test "${MINION_JOIN+x}" != x && test "${MINION_INVITE_KEY+x}" != x &&
test "$#" = 5 && test "$1" = worker && test "$2" = status &&
test "$3" = --state-dir && test "$4" = "$HOME/a profile" && test "$5" = --json || exit 2
printf '%s\n' '{"mode":"solo","connection":{"live":true}}'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINION_JOIN", "test token")
	t.Setenv("MINION_INVITE_KEY", "test invitation")
	owner := &ownerSetup{Target: service.Target{User: "owner", UID: os.Getuid(), GID: os.Getgid(), Home: home}, StateDir: dir, Binary: bin}
	report, err := readUpStatus(context.Background(), dir, owner)
	if err != nil || !report.Connection.Live {
		t.Fatalf("owner status = %+v, %v", report, err)
	}
}
