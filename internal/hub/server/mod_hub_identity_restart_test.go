package server

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/adminsock"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

// F1: with the full default module registry, set-passphrase on a pristine
// Hub creates the sealed identity without any plain identity.key, and the next
// start of the Hub must still work: the identity service is opened once (by
// the "hubid" module) and the workers and hub_identity modules consume it.
func TestHubStartsAgainAfterSetPassphraseWithTheDefaultRegistry(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "identity-integration-pw-1")
	sealer := newIDFakeSealer()
	identityOptions = func(o *hubid.Options) { o.Sealer = sealer; o.LockWait = 500 * time.Millisecond }
	t.Cleanup(func() { identityOptions = nil })

	dir := shortDir(t)
	h := startHubPristine(t, &Config{DataDir: dir}, DefaultRegistry)
	// A pristine Hub (B0) creates no key: it waits for the passphrase.
	if _, err := os.Stat(filepath.Join(dir, workers.IdentityFile)); !os.IsNotExist(err) {
		t.Fatalf("a fresh Hub created a plain identity.key: %v", err)
	}
	ih := &idHub{dir: dir}
	code, body := ih.socket(t, "POST", adminsock.PathSetPassphrase, map[string]any{"passphrase": idPass1, "confirm": idPass1, "validity_days": 30})
	if code != 200 {
		t.Fatalf("set-passphrase = %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(dir, workers.IdentityFile)); err == nil {
		t.Fatal("set-passphrase left a plain identity.key")
	}
	h.stop(t)

	h2 := startHubPristine(t, &Config{DataDir: dir}, DefaultRegistry)
	code, body = ih.socket(t, "GET", adminsock.PathStatus, nil)
	if code != 200 {
		t.Fatalf("status after restart = %d %s", code, body)
	}
	h2.stop(t)
	if _, err := os.Stat(filepath.Join(dir, workers.IdentityFile)); err == nil {
		t.Error("the restart re-created identity.key")
	}
}

// One owner of the identity: "hubid" opens it, the workers module consumes it,
// and the operations module comes after both.
func TestIdentityModulesAreOrderedOwnerWorkersOperations(t *testing.T) {
	names := DefaultRegistry.Names()
	owner, wk, opsMod := slices.Index(names, "hubid"), slices.Index(names, "workers"), slices.Index(names, "hub_identity")
	if owner < 0 || wk < 0 || opsMod < 0 || owner >= wk || wk >= opsMod {
		t.Errorf("modules = %v", names)
	}
}
