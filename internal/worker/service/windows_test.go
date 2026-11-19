package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

func TestPipeUserSIDEnvIsTheOneTheWorkerReads(t *testing.T) {
	if pipeUserSIDEnv != localapi.PipeUserSIDEnv {
		t.Fatalf("service writes %q, the Worker reads %q", pipeUserSIDEnv, localapi.PipeUserSIDEnv)
	}
	if pipeUserSIDEnv != identity.PipeUserSIDEnv {
		t.Fatalf("service writes %q, the state protection reads %q", pipeUserSIDEnv, identity.PipeUserSIDEnv)
	}
}

// errDenied stands for ERROR_ACCESS_DENIED with a message in another language.
var errDenied = errors.New("zugriff verweigert")

// denyAccess makes the install error hint treat errDenied as access denied for
// the rest of the test; the real check is by Windows error number.
func denyAccess(t *testing.T) {
	t.Helper()
	old := accessDenied
	accessDenied = func(err error) bool { return errors.Is(err, errDenied) }
	t.Cleanup(func() { accessDenied = old })
}

func TestWindowsInstallErrorExplainsAccessDenied(t *testing.T) {
	denyAccess(t)
	err := windowsInstallError("install", errDenied)
	for _, want := range []string{"install the service", "zugriff verweigert", "administrator", "sc.exe delete MinionWorker"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	plain := windowsInstallError("start", errors.New("boom"))
	if strings.Contains(plain.Error(), "administrator") || !strings.Contains(plain.Error(), "start the service: boom") {
		t.Errorf("plain error = %v", plain)
	}
}

// The English text is not what identifies the error: without the error number
// the hint is absent, whatever the message says.
func TestWindowsInstallErrorIgnoresTheMessageText(t *testing.T) {
	denyAccess(t)
	err := windowsInstallError("install", errors.New("Access is denied."))
	if strings.Contains(err.Error(), "administrator") {
		t.Errorf("the hint was added for a message only: %v", err)
	}
}
