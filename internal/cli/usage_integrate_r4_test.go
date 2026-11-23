package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// A conflict found after our replacement was renamed into place must keep the
// manifest the installed hooks reference; a later pass is then a no-op.
func TestPostCommitSettingsConflictKeepsTheManifest(t *testing.T) {
	_, profile, state, _ := exporterProfile(t, `{"permissions":{}}`)
	m := claudeUsageIntegration{Version: 1, Binary: "/bin/minion", StateDir: state, Shell: "/bin/sh"}
	old := writeClaudeSettingsFile
	t.Cleanup(func() { writeClaudeSettingsFile = old })
	writeClaudeSettingsFile = func(p string, data []byte, check func() error) error {
		if err := identity.WriteReplacing(p, data, check); err != nil {
			return err
		}
		// Claude saves an unrelated change that keeps our hooks.
		mustWrite(t, p, bytes.Replace(data, []byte(`"permissions"`), []byte(`"other":1,"permissions"`), 1))
		return &identity.ConflictError{Path: p, Backup: p + identity.BackupSuffix}
	}
	_, err := installClaudeUsage(context.Background(), profile, m, nil)
	if !errors.Is(err, errClaudeSettingsConflict) {
		t.Fatalf("err %v", err)
	}
	if !bytes.Contains(readClaudeFixture(t, profile, "settings.json"), []byte(claudeUsageMarker)) {
		t.Fatal("test setup: the hooks are not in the settings")
	}
	if _, err := os.Stat(filepath.Join(profile, claudeUsageManifest)); err != nil {
		t.Fatalf("the manifest the installed hooks reference was deleted: %v", err)
	}
}

// Our status line present but the manifest missing is repaired, not refused.
func TestInstallRecreatesAMissingManifestOfOurOwnWrapper(t *testing.T) {
	_, profile, state, _ := exporterProfile(t, `{"permissions":{}}`)
	m := claudeUsageIntegration{Version: 1, Binary: "/bin/minion", StateDir: state, Shell: "/bin/sh"}
	if _, err := installClaudeUsage(context.Background(), profile, m, nil); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(profile, claudeUsageManifest)
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	before := readClaudeFixture(t, profile, "settings.json")
	changed, err := installClaudeUsage(context.Background(), profile, m, nil)
	if err != nil || !changed {
		t.Fatalf("self-heal: %t %v", changed, err)
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("manifest not recreated: %v", err)
	}
	if !bytes.Equal(readClaudeFixture(t, profile, "settings.json"), before) {
		t.Fatal("the settings were rewritten")
	}
	if changed, err := installClaudeUsage(context.Background(), profile, m, nil); err != nil || changed {
		t.Fatalf("not idempotent: %t %v", changed, err)
	}
}
