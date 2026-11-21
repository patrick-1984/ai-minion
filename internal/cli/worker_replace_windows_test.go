//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func TestReplaceWindowsRollbackRetainsPrivateACL(t *testing.T) {
	parent := t.TempDir()
	state := filepath.Join(parent, "state")
	holder := filepath.Join(parent, "backup")
	original := filepath.Join(holder, "original")
	for _, dir := range []string{state, holder} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ProtectStateDir(state); err != nil {
		t.Fatal(err)
	}
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(state, agent.WorkerKeyFile)
	if err := identity.SaveKey(keyPath, key); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(state, original); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(original, state); err != nil {
		t.Fatal(err)
	}
	if err := restoreTree(original, state); err != nil {
		t.Fatal(err)
	}
	restored, err := identity.LoadKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Fingerprint() != key.Fingerprint() {
		t.Fatal("rollback changed the Worker's identity")
	}
}
