//go:build linux

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginChildHasNoCredentialsOrTerminal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("TMPDIR", dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("MINION_JOIN", "never-forward-this")
	t.Setenv("MINION_INVITE_KEY_FILE", "never-forward-this")
	t.Setenv("OTHER_TOKEN", "never-forward-this")
	bin := filepath.Join(dir, "binary with spaces")
	script := `#!/bin/sh
[ "$MINION_DETACHED" = 1 ] && [ ! -t 0 ] && [ "$#" = 4 ] &&
[ "$1" = worker ] && [ "$2" = run ] && [ "$3" = --state-dir ] && [ "$4" = "$PWD" ] &&
[ -z "${MINION_JOIN+x}${MINION_INVITE_KEY_FILE+x}${OTHER_TOKEN+x}" ] &&
[ "$(ps -o sid= -p "$$" | tr -d ' ')" = "$$" ] && echo detached
exit 42
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	err := spawnLoginWorker(context.Background(), bin, dir)
	if err == nil || !strings.Contains(err.Error(), "exit status 42") || !strings.Contains(err.Error(), launchLogName) {
		t.Fatalf("early exit = %v", err)
	}
	path := filepath.Join(dir, launchLogName)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("log: %v, %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "detached\n" {
		t.Fatalf("child failed isolation checks: %q, %v", data, err)
	}
}
