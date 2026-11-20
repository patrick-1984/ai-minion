package cli

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// Docker enrolls once in a disposable container. Its normal, independently
// supervised process must be able to start solely from the saved state.
func TestWorkerUpConfigureOnlySavesSettingsWithoutStarting(t *testing.T) {
	f, _ := serviceGate(t, false, false, "build")
	f.deps.detectEnv.Exists = func(string) bool { return false }
	var seen joinSeen
	joinHook(t, f, &seen)
	token, join := testJoinToken(t, "https://hub.example:8443")
	_, err := stdinJoin(t, f, strings.NewReader(token+"\n"), "--configure-only", "--share-existing", "later")
	if err != nil {
		t.Fatal(err)
	}
	config := readConfig(t, f.stateDir())
	if seen.invite.Value != join.Invite.String() || config["server"] != join.HubURL || config["hub_fingerprint"] != join.Fingerprint().String() {
		t.Fatal("the invitation hook did not run or the Hub settings were not saved")
	}
	if _, saved := config["invite_key"]; saved {
		t.Fatal("the invitation key was saved")
	}
	if calls := f.runCalls(); len(calls) != 0 {
		t.Fatalf("service calls: %v", calls)
	}
	if _, err := f.run("--configure-only", "--foreground"); err == nil {
		t.Fatal("--configure-only and --foreground both accepted")
	}
}

func TestConfigureOnlyLeavesWindowsLegacyServiceIntact(t *testing.T) {
	f, w := newWinUp(t)
	w.elev = service.Elevation{Admin: true, Elevated: true}
	w.legacy = service.WindowsServiceState{Installed: true, Account: "LocalSystem"}
	if _, err := f.run("--configure-only", "--share-existing", "none"); err == nil || len(w.events) != 0 {
		t.Fatalf("configure-only with a legacy Windows service: err = %v, events = %v", err, w.events)
	}
}
